package server

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"time"

	"github.com/crewjam/saml"
	logrus "github.com/sirupsen/logrus"

	"github.com/pachyderm/pachyderm/src/client/auth"
	"github.com/pachyderm/pachyderm/src/server/pkg/backoff"
	"github.com/pachyderm/pachyderm/src/server/pkg/watch"
)

// configSource indicates whether a pachyderm auth config was received from a
// caller of the SetAuthConfig API or read from etcd. In the first case, we
// should canonicalize the request's config, and in the second, the
// configuration should already have been validated and any non-canonical
// configuration should yield an error
type configSource uint8

const (
	internal configSource = iota
	external
)

type canonicalSAMLIDP struct {
	MetadataURL    *url.URL
	Metadata       *saml.EntityDescriptor
	GroupAttribute string
}

type canonicalIDPConfig struct {
	Name        string
	Description string

	SAML *canonicalSAMLIDP
}

type canonicalSAMLSvcConfig struct {
	ACSURL          *url.URL
	MetadataURL     *url.URL
	DashURL         *url.URL      // optional (use defaultDashRedirectURL if unset)
	SessionDuration time.Duration // optional
}

// canonicalConfig contains the values specified in an auth.AuthConfig proto
// message, but as structured Go types. This is populated and returned by
// validateConfig
type canonicalConfig struct {
	Version int64
	Source  configSource

	// currently, there is only one permissible type of ID provider (SAML), and
	// SAMLSvc must be set iff there is a SAML ID provider in this list. Therefore
	// there are currently two possible forms of canonicalConfig:
	// 1. empty config
	// 2. IDPs contains a single element configuring a SAML ID provider, and
	//    SAMLSvc contains config for Pachyderm's ACS
	IDPs []canonicalIDPConfig

	// SAMLSvc must be set
	SAMLSvc *canonicalSAMLSvcConfig
}

func (c *canonicalConfig) ToProto() (*auth.AuthConfig, error) {
	// ToProto may be called on an empty canonical config if the user is setting
	// an empty config (the empty AuthConfig proto will be validated and then
	// reverted to a proto before being written to etcd)
	if c.IsEmpty() {
		return &auth.AuthConfig{}, nil
	}

	var idpProtos []*auth.IDProvider
	for _, idp := range c.IDPs {
		if idp.SAML == nil {
			return nil, fmt.Errorf("could not marshal non-SAML ID provider %q", idp.Name)
		}
		metadataBytes, err := xml.MarshalIndent(idp.SAML.Metadata, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("could not marshal ID provider metadata: %v", err)
		}
		samlIDP := &auth.IDProvider{
			Name:        idp.Name,
			Description: idp.Description,
			SAML: &auth.IDProvider_SAMLOptions{
				MetadataXML:    metadataBytes,
				GroupAttribute: idp.SAML.GroupAttribute,
			},
		}
		if idp.SAML.MetadataURL != nil {
			samlIDP.SAML.MetadataURL = idp.SAML.MetadataURL.String()
		}
		idpProtos = append(idpProtos, samlIDP)
	}

	var svcCfgProto *auth.AuthConfig_SAMLServiceOptions
	if c.SAMLSvc != nil {
		svcCfgProto = &auth.AuthConfig_SAMLServiceOptions{
			ACSURL:      c.SAMLSvc.ACSURL.String(),
			MetadataURL: c.SAMLSvc.MetadataURL.String(),
		}
		if c.SAMLSvc.DashURL != nil {
			svcCfgProto.DashURL = c.SAMLSvc.DashURL.String()
		}
		if c.SAMLSvc.SessionDuration > 0 {
			svcCfgProto.SessionDuration = c.SAMLSvc.SessionDuration.String()
		}
	}

	return &auth.AuthConfig{
		IDProviders:        idpProtos,
		SAMLServiceOptions: svcCfgProto,
	}, nil
}

func (c *canonicalConfig) IsEmpty() bool {
	return c == nil || len(c.IDPs) == 0
}

// fetchRawIDPMetadata is a helper of validateIDP, below. It takes the URL of a
// SAML ID provider's Metadata service, queries it, parses the result, and
// returns it as a struct the crewjam/saml library can use.  This code is
// heavily based on the crewjam/saml/samlsp.Middleware constructor
func fetchRawIDPMetadata(name string, mdURL *url.URL) ([]byte, error) {
	c := http.DefaultClient
	req, err := http.NewRequest("GET", mdURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("could not retrieve IdP metadata for %q: %v", name, err)
	}
	req.Header.Set("User-Agent", "Golang; github.com/pachyderm/pachdyerm")

	var rawMetadata []byte
	b := backoff.NewInfiniteBackOff()
	b.MaxElapsedTime = 30 * time.Second
	b.MaxInterval = 2 * time.Second
	if err := backoff.RetryNotify(func() error {
		resp, err := c.Do(req)
		if err != nil {
			return err
		}

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%d %s", resp.StatusCode, resp.Status)
		}
		rawMetadata, err = ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("could not read IdP metadata response body: %v", err)
		}
		if len(rawMetadata) == 0 {
			return fmt.Errorf("empty metadata from IdP")
		}
		return nil
	}, b, func(err error, d time.Duration) error {
		logrus.Printf("error retrieving IdP metadata: %v; retrying in %v", err, d)
		return nil
	}); err != nil {
		return nil, err
	}

	// Successfully retrieved metadata
	return rawMetadata, nil
}

// validateIDP is a helper for validateConfig, that validates each ID provider
// in the config
func validateIDP(idp *auth.IDProvider, src configSource) (*canonicalIDPConfig, error) {
	// Validate the ID Provider's name (must exist and must not be reserved)
	if idp.Name == "" {
		return nil, errors.New("All ID providers must have a name specified (for " +
			"use during authorization)")
	}
	// TODO(msteffen): make sure we don't have to extend this every time we add
	// a new built-in backend.
	switch idp.Name + ":" {
	case auth.GitHubPrefix:
		return nil, errors.New("cannot configure ID provider with reserved prefix " +
			auth.GitHubPrefix)
	case auth.RobotPrefix:
		return nil, errors.New("cannot configure ID provider with reserved prefix " +
			auth.RobotPrefix)
	case auth.PipelinePrefix:
		return nil, errors.New("cannot configure ID provider with reserved prefix " +
			auth.PipelinePrefix)
	}

	// Check if the IDP is a known type (right now the only type of IDP is SAML)
	if idp.SAML == nil {
		// render ID provider as json for error message
		idpConfigAsJSON, err := json.MarshalIndent(idp, "", "  ")
		idpConfigMsg := string(idpConfigAsJSON)
		if err != nil {
			idpConfigMsg = fmt.Sprintf("(could not marshal config json: %v)", err)
		}
		return nil, fmt.Errorf("ID provider has unrecognized type: %v", idpConfigMsg)
	}
	newIDP := &canonicalIDPConfig{}
	newIDP.Name = idp.Name
	newIDP.Description = idp.Description
	newIDP.SAML = &canonicalSAMLIDP{
		GroupAttribute: idp.SAML.GroupAttribute,
	}

	// construct this SAML ID provider's metadata. There are three valid cases:
	// 1. This is a user-provided config (i.e. it's coming from an RPC), and the
	//    IDP's metadata was set directly in the config
	// 2. This is a user-provided config, and the IDP's metadata was not set
	//    in the config, but the config contains a URL where the IDP metadata
	//    can be retrieved
	// 3. This is an internal config (it has already been validated by a pachd
	//    worker, and it's coming from etcd)
	// Any other case should be rejected with an error
	//
	// Either download raw IDP metadata from metadata URL or get it from cfg
	var rawIDPMetadata []byte
	if idp.SAML.MetadataURL == "" {
		if len(idp.SAML.MetadataXML) == 0 {
			return nil, fmt.Errorf("must set either metadata_xml or metadata_url "+
				"for the SAML ID provider %q", idp.Name)
		}
		rawIDPMetadata = idp.SAML.MetadataXML
	} else {
		// Parse URL even if this is an internal cfg and IDPMetadata is already
		// set, so that GetConfig can return it
		var err error
		newIDP.SAML.MetadataURL, err = url.Parse(idp.SAML.MetadataURL)
		if err != nil {
			return nil, fmt.Errorf("Could not parse SAML IDP metadata URL (%q) to "+
				"query it: %v", idp.SAML.MetadataURL, err)
		} else if newIDP.SAML.MetadataURL.Scheme == "" {
			return nil, fmt.Errorf("SAML IDP metadata URL %q is invalid (no scheme)",
				idp.SAML.MetadataURL)
		}

		switch src {
		case external: // user-provided config
			if len(idp.SAML.MetadataXML) > 0 {
				return nil, fmt.Errorf("cannot set both metadata_xml and metadata_url "+
					"for the SAML ID provider %q", idp.Name)
			}
			rawIDPMetadata, err = fetchRawIDPMetadata(idp.Name, newIDP.SAML.MetadataURL)
			if err != nil {
				return nil, err
			}

		case internal: // config from etcd
			if len(idp.SAML.MetadataXML) == 0 {
				return nil, fmt.Errorf("internal error: the SAML ID provider %q was "+
					"persisted without IDP metadata", idp.Name)
			}
			rawIDPMetadata = idp.SAML.MetadataXML
		}
	}

	// Parse IDP metadata. This code is heavily based on the
	// crewjam/saml/samlsp.Middleware constructor
	newIDP.SAML.Metadata = &saml.EntityDescriptor{}
	err := xml.Unmarshal(rawIDPMetadata, newIDP.SAML.Metadata)
	if err != nil {
		// this comparison is ugly, but it is how the error is generated in
		// encoding/xml
		if err.Error() != "expected element type <EntityDescriptor> but have <EntitiesDescriptor>" {
			return nil, fmt.Errorf("could not unmarshal EntityDescriptor from IDP metadata: %v", err)
		}
		// Search through <EntitiesDescriptor> & find IDP entity
		entities := &saml.EntitiesDescriptor{}
		if err := xml.Unmarshal(rawIDPMetadata, entities); err != nil {
			return nil, fmt.Errorf("could not unmarshal EntitiesDescriptor from IDP metadata: %v", err)
		}
		for i, e := range entities.EntityDescriptors {
			if len(e.IDPSSODescriptors) > 0 {
				newIDP.SAML.Metadata = &entities.EntityDescriptors[i]
				break
			}
		}
		// Make sure we found an IDP entity descriptor
		if len(newIDP.SAML.Metadata.IDPSSODescriptors) == 0 {
			return nil, fmt.Errorf("no entity found with IDPSSODescriptor")
		}
	}
	return newIDP, nil
}

// validateConfig converts an auth.AuthConfig proto from an RPC into a
// canonicalized config (with all URLs parsed, SAML metadata fetched and
// persisted, etc.)
func validateConfig(config *auth.AuthConfig, src configSource) (*canonicalConfig, error) {
	if config == nil {
		config = &auth.AuthConfig{}
	}
	c := &canonicalConfig{
		Version: config.LiveConfigVersion,
	}
	var err error

	// Validate all ID providers (and fetch IDP metadata for all SAML ID
	// providers)
	var samlIDP string
	for _, idp := range config.IDProviders {
		if idp.SAML != nil {
			// confirm that there is only one SAML IDP (requirement for now)
			if samlIDP != "" {
				return nil, fmt.Errorf("two SAML providers found in config, %q and %q, "+
					"but only one is allowed", idp.Name, samlIDP)
			}
			samlIDP = idp.Name
		}
		canonicalIDP, err := validateIDP(idp, src)
		if err != nil {
			return nil, err
		}
		c.IDPs = append(c.IDPs, *canonicalIDP)
	}

	// Make sure a SAML ID provider is configured if using SAML
	if samlIDP == "" && config.SAMLServiceOptions != nil {
		return nil, errors.New("cannot set saml_svc_options without configuring a SAML ID provider")
	}
	// Make sure saml_svc_options are set if using SAML
	if samlIDP != "" && config.SAMLServiceOptions == nil {
		return nil, errors.New("must set saml_svc_options if a SAML ID provider has been configured")
	}

	// Validate saml_svc_options
	if config.SAMLServiceOptions != nil {
		svcCfgProto := config.SAMLServiceOptions
		c.SAMLSvc = &canonicalSAMLSvcConfig{}
		// parse ACS URL
		if svcCfgProto.ACSURL == "" {
			return nil, errors.New("invalid SAML service options: must set ACS URL")
		}
		if c.SAMLSvc.ACSURL, err = url.Parse(svcCfgProto.ACSURL); err != nil {
			return nil, fmt.Errorf("could not parse SAML config ACS URL (%q): %v",
				svcCfgProto.ACSURL, err)
		} else if c.SAMLSvc.ACSURL.Scheme == "" {
			return nil, fmt.Errorf("ACS URL %q is invalid (no scheme)", svcCfgProto.ACSURL)
		}

		// parse Metadata URL
		if svcCfgProto.MetadataURL == "" {
			return nil, errors.New("invalid SAML service options: must set Metadata URL")
		}
		if c.SAMLSvc.MetadataURL, err = url.Parse(svcCfgProto.MetadataURL); err != nil {
			return nil, fmt.Errorf("could not parse SAML config metadata URL (%q): %v",
				svcCfgProto.MetadataURL, err)
		} else if c.SAMLSvc.MetadataURL.Scheme == "" {
			return nil, fmt.Errorf("Metadata URL %q is invalid (no scheme)", svcCfgProto.MetadataURL)
		}

		// parse Dash URL
		if svcCfgProto.DashURL != "" {
			if c.SAMLSvc.DashURL, err = url.Parse(svcCfgProto.DashURL); err != nil {
				return nil, fmt.Errorf("could not parse Pachyderm dashboard URL (%q): %v", svcCfgProto.DashURL, err)
			} else if c.SAMLSvc.DashURL.Scheme == "" {
				return nil, fmt.Errorf("Pachyderm dashboard URL %q is invalid (no scheme)", svcCfgProto.DashURL)
			}
		}

		// parse session duration
		if svcCfgProto.SessionDuration != "" {
			c.SAMLSvc.SessionDuration, err = time.ParseDuration(svcCfgProto.SessionDuration)
			if err != nil {
				return nil, fmt.Errorf("could not parse SAML-based session duration: %v", err)
			}
		}
	}

	return c, nil
}

// setCacheConfig validates 'config', and if it valides successfully, loads it
// into the apiServer's config cache. The caller should already hold a.configMu
// and a.samlSPMu (as this updates a.samlSP)
func (a *apiServer) setCacheConfig(config *auth.AuthConfig) error {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	a.samlSPMu.Lock()
	defer a.samlSPMu.Unlock()
	if config == nil {
		logrus.Warnf("deleting the cached config, but it should not be possible " +
			"to delete the auth config in etcd without deactivating auth. Is that " +
			"what's happening?")
		a.configCache = nil
		a.samlSP = nil
		return nil
	}

	newConfig, err := validateConfig(config, internal)
	if err != nil {
		return err
	}
	if a.configCache != nil {
		if newConfig.Version < a.configCache.Version {
			return fmt.Errorf("new config has lower version than cached config (%d < %d)",
				newConfig.Version, a.configCache.Version)
		} else if newConfig.Version == a.configCache.Version {
			// This shouldn't happen, but can if a user calls GetConfiguration and it
			// races with watchConfig. Just log the two configs and continue
			logrus.Warnf("new config has same version as cached config:%+v\nand:\n%+v\n",
				newConfig.Version, a.configCache)
		}
	}

	// Set a.configCache and possibly a.samlSP
	a.configCache = newConfig
	a.samlSP = nil // overwrite if there's a SAML ID provider
	for _, idp := range newConfig.IDPs {
		if idp.SAML != nil {
			a.samlSP = &saml.ServiceProvider{
				Logger:      logrus.New(),
				IDPMetadata: idp.SAML.Metadata,
				AcsURL:      *newConfig.SAMLSvc.ACSURL,
				MetadataURL: *newConfig.SAMLSvc.MetadataURL,

				// Not set:
				// Key: Private key for Pachyderm ACS. Unclear if needed
				// Certificate: Public key for Pachyderm ACS. Unclear if needed
				// ForceAuthn: (whether users need to re-authenticate with the IdP, even
				//             if they already have a session--leaving this false)
				// AuthnNameIDFormat: (format the ACS expects the AuthnName to be in)
				// MetadataValidDuration: (how long the SP endpoints are valid? Returned
				//                        by the Metadata service)
			}
		}
	}
	return nil
}

func (a *apiServer) getCacheConfig() *canonicalConfig {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	if a.configCache == nil {
		return nil
	}
	// copy config to avoid data races
	newConfig := *a.configCache
	return &newConfig
}

// getSAMLSP returns apiServer's saml.ServiceProvider and config together, to
// avoid a race where a SAML request is mishandled because the config is
// modified between reading them
func (a *apiServer) getSAMLSP() (*canonicalConfig, *saml.ServiceProvider) {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	a.samlSPMu.Lock()
	defer a.samlSPMu.Unlock()
	var sp *saml.ServiceProvider
	if a.samlSP != nil {
		sp = a.samlSP
	}
	var cfg *canonicalConfig
	if a.configCache != nil {
		cfg = a.configCache
	}
	// copy config to avoid data races
	return &(*cfg), &(*sp)
}

// watchConfig waits for config updates in etcd and then copies new config
// values into the confg cache
func (a *apiServer) watchConfig() {
	b := backoff.NewExponentialBackOff()
	backoff.RetryNotify(func() error {
		// Watch for the addition/removal of new admins. Note that this will return
		// any existing admins, so if the auth service is already activated, it will
		// stay activated.
		watcher, err := a.authConfig.ReadOnly(context.Background()).Watch()
		if err != nil {
			return err
		}
		defer watcher.Close()
		// Wait for new config events to arrive
		for {
			ev, ok := <-watcher.Watch()
			if !ok {
				return errors.New("admin watch closed unexpectedly")
			}
			b.Reset() // event successfully received

			if a.activationState() != full {
				return fmt.Errorf("received config event while auth not fully " +
					"activated (should be impossible), restarting")
			}
			if err := func() error {
				// Parse event data and potentially update configCache
				var key string // always configKey, just need to put it somewhere
				var configProto auth.AuthConfig
				ev.Unmarshal(&key, &configProto)
				switch ev.Type {
				case watch.EventPut:
					if err := a.setCacheConfig(&configProto); err != nil {
						logrus.Warnf("could not update SAML service with new config: %v", err)
					}
				case watch.EventDelete:
					// This should currently be impossible
					logrus.Warnf("auth config has been deleted: possible internal error")
					a.setCacheConfig(nil)
				case watch.EventError:
					return ev.Err
				}
				return nil // unlock configMu and samlSPMu
			}(); err != nil {
				return err
			}
		}
	}, b, func(err error, d time.Duration) error {
		logrus.Errorf("error watching auth config: %v; retrying in %v", err, d)
		return nil
	})
}
