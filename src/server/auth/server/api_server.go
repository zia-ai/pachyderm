package server

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/crewjam/saml"
	"github.com/gogo/protobuf/proto"
	"github.com/gogo/protobuf/types"
	"github.com/google/go-github/github"
	logrus "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/metadata"

	"github.com/pachyderm/pachyderm/src/client/auth"
	enterpriseclient "github.com/pachyderm/pachyderm/src/client/enterprise"
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pkg/grpcutil"
	"github.com/pachyderm/pachyderm/src/client/pps"
	"github.com/pachyderm/pachyderm/src/server/pkg/backoff"
	col "github.com/pachyderm/pachyderm/src/server/pkg/collection"
	"github.com/pachyderm/pachyderm/src/server/pkg/log"
	"github.com/pachyderm/pachyderm/src/server/pkg/ppsconsts"
	"github.com/pachyderm/pachyderm/src/server/pkg/serviceenv"
	txnenv "github.com/pachyderm/pachyderm/src/server/pkg/transactionenv"
	"github.com/pachyderm/pachyderm/src/server/pkg/uuid"
	"github.com/pachyderm/pachyderm/src/server/pkg/watch"
)

const (
	// DisableAuthenticationEnvVar specifies an environment variable that, if set, causes
	// Pachyderm authentication to ignore github and authmatically generate a
	// pachyderm token for any username in the AuthenticateRequest.GitHubToken field
	DisableAuthenticationEnvVar = "PACHYDERM_AUTHENTICATION_DISABLED_FOR_TESTING"

	tokensPrefix              = "/tokens"
	authenticationCodesPrefix = "/auth-codes"
	aclsPrefix                = "/acls"
	adminsPrefix              = "/admins"
	membersPrefix             = "/members"
	groupsPrefix              = "/groups"
	configPrefix              = "/config"

	defaultTokenTTLSecs = 30 * 24 * 60 * 60 // 30 days
	defaultSAMLTTLSecs  = 24 * 60 * 60      // 24 hours

	// defaultAuthCodeTTLSecs is the lifetime of an Authentication Code from
	// GetOneTimePassword
	defaultAuthCodeTTLSecs = 120

	// magicUser is a special, unrevokable cluster administrator. It's not
	// possible to log in as magicUser, but pipelines with no owner are run as
	// magicUser when auth is activated. This string is not secret, but is long
	// and random to avoid collisions with real usernames
	magicUser = `magic:GZD4jKDGcirJyWQt6HtK4hhRD6faOofP1mng34xNZsI`

	// configKey is a key (in etcd, in the config collection) that maps to the
	// auth configuration. This is the only key in that collection (due to
	// implemenation details of our config library, we can't use an empty key)
	configKey = "config"

	// SamlPort is the port where SAML ID Providers can send auth assertions
	SamlPort = 654
)

var defaultAuthConfig = auth.AuthConfig{
	LiveConfigVersion: 1,
}

// githubTokenRegex is used when pachd is deployed in "dev mode" (i.e. when
// pachd is deployed with "pachctl deploy local") to guess whether a call to
// Authenticate or Authorize contains a real GitHub access token.
//
// If the field GitHubToken matches this regex, it's assumed to be a GitHub
// token and pachd retrieves the corresponding user's GitHub username. If not,
// pachd automatically authenticates the the caller as the GitHub user whose
// username is the string in "GitHubToken".
var githubTokenRegex = regexp.MustCompile("^[0-9a-f]{40}$")

// epsilon is small, nonempty protobuf to use as an etcd value (the etcd client
// library can't distinguish between empty values and missing values, even
// though empty values are still stored in etcd)
var epsilon = &types.BoolValue{Value: true}

// APIServer represents an auth api server
type APIServer interface {
	auth.APIServer
	txnenv.AuthTransactionServer
}

// apiServer implements the public interface of the Pachyderm auth system,
// including all RPCs defined in the protobuf spec.
type apiServer struct {
	env        *serviceenv.ServiceEnv
	txnEnv     *txnenv.TransactionEnv
	pachLogger log.Logger

	adminCache map[string]struct{} // cache of current cluster admins
	adminMu    sync.Mutex          // guard 'adminCache'

	// configCache should not be read/written directly--use setCacheConfig and
	// getCacheConfig
	configCache *canonicalConfig // cache of auth config in etcd
	configMu    sync.Mutex       // guard 'configCache'. Always lock before 'samlSPMu' (if using both)

	// samlSP should not be read/written directly--use setCacheConfig and
	// getSAMLSP
	samlSP   *saml.ServiceProvider // object for parsing saml responses
	samlSPMu sync.Mutex            // guard 'samlSP'. Always lock after 'configMu' (if using both)

	// tokens is a collection of hashedToken -> TokenInfo mappings. These tokens are
	// returned to users by Authenticate()
	tokens col.Collection
	// authenticationCodes is a collection of hash(code) -> TokenInfo mappings.
	// These codes are generated internally, and converted to regular tokens by
	// Authenticate()
	authenticationCodes col.Collection
	// acls is a collection of repoName -> ACL mappings.
	acls col.Collection
	// admins is a collection of username -> Empty mappings (keys indicate which
	// github users are cluster admins)
	admins col.Collection
	// members is a collection of username -> groups mappings.
	members col.Collection
	// groups is a collection of group -> usernames mappings.
	groups col.Collection
	// collection containing the auth config (under the key configKey)
	authConfig col.Collection

	// This is a cache of the PPS master token. It's set once on startup and then
	// never updated
	ppsToken string

	// public addresses the fact that pachd in full mode initializes two auth
	// servers: one that exposes a public API, possibly over TLS, and one that
	// exposes a private API, for internal services. Only the public-facing auth
	// service should export the SAML ACS and Metadata services, so if public
	// is true and auth is active, this may export those SAML services
	public bool
}

// LogReq is like log.Logger.Log(), but it assumes that it's being called from
// the top level of a GRPC method implementation, and correspondingly extracts
// the method name from the parent stack frame
func (a *apiServer) LogReq(request interface{}) {
	a.pachLogger.Log(request, nil, nil, 0)
}

// LogResp is like log.Logger.Log(). However,
// 1) It assumes that it's being called from a defer() statement in a GRPC
//    method , and correspondingly extracts the method name from the grandparent
//    stack frame
// 2) It logs NotActivatedError at DebugLevel instead of ErrorLevel, as, in most
//    cases, this error is expected, and logging it frequently may confuse users
func (a *apiServer) LogResp(request interface{}, response interface{}, err error, duration time.Duration) {
	if err == nil {
		a.pachLogger.LogAtLevelFromDepth(request, response, err, duration, logrus.InfoLevel, 4)
	} else if auth.IsErrNotActivated(err) {
		a.pachLogger.LogAtLevelFromDepth(request, response, err, duration, logrus.DebugLevel, 4)
	} else {
		a.pachLogger.LogAtLevelFromDepth(request, response, err, duration, logrus.ErrorLevel, 4)
	}
}

// NewAuthServer returns an implementation of auth.APIServer.
func NewAuthServer(
	env *serviceenv.ServiceEnv,
	txnEnv *txnenv.TransactionEnv,
	etcdPrefix string,
	public bool,
) (APIServer, error) {
	s := &apiServer{
		env:        env,
		txnEnv:     txnEnv,
		pachLogger: log.NewLogger("auth.API"),
		adminCache: make(map[string]struct{}),
		tokens: col.NewCollection(
			env.GetEtcdClient(),
			path.Join(etcdPrefix, tokensPrefix),
			nil,
			&auth.TokenInfo{},
			nil,
			nil,
		),
		authenticationCodes: col.NewCollection(
			env.GetEtcdClient(),
			path.Join(etcdPrefix, authenticationCodesPrefix),
			nil,
			&auth.OTPInfo{},
			nil,
			nil,
		),
		acls: col.NewCollection(
			env.GetEtcdClient(),
			path.Join(etcdPrefix, aclsPrefix),
			nil,
			&auth.ACL{},
			nil,
			nil,
		),
		admins: col.NewCollection(
			env.GetEtcdClient(),
			path.Join(etcdPrefix, adminsPrefix),
			nil,
			&types.BoolValue{}, // smallest value that etcd actually stores
			nil,
			nil,
		),
		members: col.NewCollection(
			env.GetEtcdClient(),
			path.Join(etcdPrefix, membersPrefix),
			nil,
			&auth.Groups{},
			nil,
			nil,
		),
		groups: col.NewCollection(
			env.GetEtcdClient(),
			path.Join(etcdPrefix, groupsPrefix),
			nil,
			&auth.Users{},
			nil,
			nil,
		),
		authConfig: col.NewCollection(
			env.GetEtcdClient(),
			path.Join(etcdPrefix, configKey),
			nil,
			&auth.AuthConfig{},
			nil,
			nil,
		),
		public: public,
	}
	go s.retrieveOrGeneratePPSToken()
	go s.watchAdmins(path.Join(etcdPrefix, adminsPrefix))

	if public {
		// start SAML service (won't respond to
		// anything until config is set)
		go s.serveSAML()
	}
	// Watch for new auth config options
	go s.watchConfig()
	return s, nil
}

type activationState int

const (
	none activationState = iota
	partial
	full
)

// activationState returns 'none' if auth is totally inactive, 'partial' if
// auth.Activate has been called, but hasn't finished or failed, and full
// if auth.Activate has been called and succeeded.
//
// When the activation state is 'partial' users cannot authenticate; the only
// functioning auth API calls are Activate() (to retry activation) and
// Deactivate() (to give up and revert to the 'none' state)
func (a *apiServer) activationState() activationState {
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	if len(a.adminCache) == 0 {
		return none
	}
	if _, magicUserIsAdmin := a.adminCache[magicUser]; magicUserIsAdmin {
		return partial
	}
	return full
}

// Retrieve the PPS master token, or generate it and put it in etcd.
// TODO This is a hack. It avoids the need to return superuser tokens from
// GetAuthToken (essentially, PPS and Auth communicate through etcd instead of
// an API) but we should define an internal API and use that instead.
func (a *apiServer) retrieveOrGeneratePPSToken() {
	var tokenProto types.StringValue // will contain PPS token
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = 60 * time.Second
	b.MaxInterval = 5 * time.Second
	if err := backoff.Retry(func() error {
		if _, err := col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
			superUserTokenCol := col.NewCollection(a.env.GetEtcdClient(), ppsconsts.PPSTokenKey, nil, &types.StringValue{}, nil, nil).ReadWrite(stm)
			// TODO(msteffen): Don't use an empty key, as it will not be erased by
			// superUserTokenCol.DeleteAll()
			err := superUserTokenCol.Get("", &tokenProto)
			if err == nil {
				return nil
			}
			if col.IsErrNotFound(err) {
				// no existing token yet -- generate token
				token := uuid.NewWithoutDashes()
				tokenProto.Value = token
				if err := superUserTokenCol.Create("", &tokenProto); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		a.ppsToken = tokenProto.Value
		return nil
	}, b); err != nil {
		panic(fmt.Sprintf("couldn't create/retrieve PPS superuser token within 60s of starting up: %v", err))
	}
}

func (a *apiServer) watchAdmins(fullAdminPrefix string) {
	b := backoff.NewExponentialBackOff()
	backoff.RetryNotify(func() error {
		// Watch for the addition/removal of new admins. Note that this will return
		// any existing admins, so if the auth service is already activated, it will
		// stay activated.
		watcher, err := a.admins.ReadOnly(context.Background()).Watch()
		if err != nil {
			return err
		}
		defer watcher.Close()
		// The auth service is activated if we have admins, and not
		// activated otherwise.
		for {
			ev, ok := <-watcher.Watch()
			if !ok {
				return errors.New("admin watch closed unexpectedly")
			}
			b.Reset() // event successfully received

			if err := func() error {
				// Lock a.adminMu in case we need to modify a.adminCache
				a.adminMu.Lock()
				defer a.adminMu.Unlock()

				// Parse event data and potentially update adminCache
				var key string
				var boolProto types.BoolValue
				ev.Unmarshal(&key, &boolProto)
				username := strings.TrimPrefix(key, fullAdminPrefix+"/")
				switch ev.Type {
				case watch.EventPut:
					a.adminCache[username] = struct{}{}
				case watch.EventDelete:
					delete(a.adminCache, username)
				case watch.EventError:
					return ev.Err
				}
				return nil // unlock mu
			}(); err != nil {
				return err
			}
		}
	}, b, func(err error, d time.Duration) error {
		logrus.Errorf("error watching admin collection: %v; retrying in %v", err, d)
		return nil
	})
}

func (a *apiServer) getEnterpriseTokenState() (enterpriseclient.State, error) {
	pachClient := a.env.GetPachClient(context.Background())
	resp, err := pachClient.Enterprise.GetState(context.Background(),
		&enterpriseclient.GetStateRequest{})
	if err != nil {
		return 0, fmt.Errorf("could not get Enterprise status: %v", grpcutil.ScrubGRPC(err))
	}
	return resp.State, nil
}

// Activate implements the protobuf auth.Activate RPC
func (a *apiServer) Activate(ctx context.Context, req *auth.ActivateRequest) (resp *auth.ActivateResponse, retErr error) {
	pachClient := a.env.GetPachClient(ctx)
	ctx = pachClient.Ctx() // copy auth information
	// We don't want to actually log the request/response since they contain
	// credentials.
	defer func(start time.Time) { a.LogResp(nil, nil, retErr, time.Since(start)) }(time.Now())
	// If the cluster's Pachyderm Enterprise token isn't active, the auth system
	// cannot be activated
	state, err := a.getEnterpriseTokenState()
	if err != nil {
		return nil, fmt.Errorf("error confirming Pachyderm Enterprise token: %v", err)
	}
	if state != enterpriseclient.State_ACTIVE {
		return nil, fmt.Errorf("Pachyderm Enterprise is not active in this " +
			"cluster, and the Pachyderm auth API is an Enterprise-level feature")
	}

	// Activating an already activated auth service should fail, because
	// otherwise anyone can just activate the service again and set
	// themselves as an admin. If activation failed in PFS, calling auth.Activate
	// again should work (in this state, the only admin will be 'magicUser')
	if a.activationState() == full {
		return nil, fmt.Errorf("already activated")
	}

	// The Pachyderm token that Activate() returns will have the TTL
	// - 'defaultTokenTTLSecs' if the initial admin is a GitHub user (who can get
	//   a new token by re-authenticating via GitHub after this token expires)
	// - 0 (no TTL, indefinite lifetime) if the initial admin is a robot user
	//   (who has no way to acquire a new token once this token expires)
	ttlSecs := int64(defaultTokenTTLSecs)
	// Authenticate the caller (or generate a new auth token if req.Subject is a
	// robot user)
	if req.Subject != "" {
		req.Subject, err = a.canonicalizeSubject(ctx, req.Subject)
		if err != nil {
			return nil, err
		}
	}
	switch {
	case req.Subject == "":
		fallthrough
	case strings.HasPrefix(req.Subject, auth.GitHubPrefix):
		username, err := GitHubTokenToUsername(ctx, req.GitHubToken)
		if err != nil {
			return nil, err
		}
		if req.Subject != "" && req.Subject != username {
			return nil, fmt.Errorf("asserted subject \"%s\" did not match owner of GitHub token \"%s\"", req.Subject, username)
		}
		req.Subject = username
	case strings.HasPrefix(req.Subject, auth.RobotPrefix):
		// req.Subject will be used verbatim, and the resulting code will
		// authenticate the holder as the robot account therein
		ttlSecs = 0 // no expiration for robot tokens -- see above
	default:
		return nil, fmt.Errorf("invalid subject in request (must be a GitHub user or robot): \"%s\"", req.Subject)
	}

	// Hack: set the cluster admins to just {magicUser}. This puts the auth system
	// in the "partial" activation state. Users cannot authenticate, but auth
	// checks are now enforced, which means no pipelines or repos can be created
	// while ACLs are being added to every repo for the existing pipelines
	if _, err = col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
		return a.admins.ReadWrite(stm).Put(magicUser, epsilon)
	}); err != nil {
		return nil, err
	}
	// wait until watchAdmins has updated the local cache (changing the activation
	// state)
	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = 30 * time.Second
	if err := backoff.Retry(func() error {
		if a.activationState() != partial {
			return fmt.Errorf("auth never entered \"partial\" activation state")
		}
		return nil
	}, b); err != nil {
		return nil, err
	}
	time.Sleep(time.Second) // give other pachd nodes time to update their cache

	// Call PPS.ActivateAuth to set up all affected pipelines and repos
	superUserClient := pachClient.WithCtx(pachClient.Ctx()) // clone pachClient
	superUserClient.SetAuthToken(a.ppsToken)
	if _, err := superUserClient.ActivateAuth(superUserClient.Ctx(), &pps.ActivateAuthRequest{}); err != nil {
		return nil, err
	}

	// Generate a new Pachyderm token (as the caller is authenticating) and
	// initialize admins (watchAdmins() above will see the write)
	pachToken := uuid.NewWithoutDashes()
	if _, err = col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
		admins := a.admins.ReadWrite(stm)
		tokens := a.tokens.ReadWrite(stm)
		if err := admins.Delete(magicUser); err != nil {
			return err
		}
		if err := admins.Put(req.Subject, epsilon); err != nil {
			return err
		}
		return tokens.PutTTL(
			hashToken(pachToken),
			&auth.TokenInfo{
				Subject: req.Subject,
				Source:  auth.TokenInfo_AUTHENTICATE,
			},
			ttlSecs,
		)
	}); err != nil {
		return nil, err
	}

	// wait until watchAdmins has updated the local cache (changing the
	// activation state), so that Activate() is less likely to race with
	// subsequent calls that expect auth to be activated.
	// TODO this is a bit hacky (checking repeatedly in a spin loop) but
	// Activate() is rarely called, and it helps avoid races due to other pachd
	// pods being out of date.
	if err := backoff.Retry(func() error {
		if a.activationState() != full {
			return fmt.Errorf("auth never entered \"full\" activation state")
		}
		return nil
	}, b); err != nil {
		return nil, err
	}
	time.Sleep(time.Second) // give other pachd nodes time to update their cache
	return &auth.ActivateResponse{PachToken: pachToken}, nil
}

// Deactivate implements the protobuf auth.Deactivate RPC
func (a *apiServer) Deactivate(ctx context.Context, req *auth.DeactivateRequest) (resp *auth.DeactivateResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.LogResp(req, resp, retErr, time.Since(start)) }(time.Now())
	if a.activationState() == none {
		// users should be able to call "deactivate" from the "partial" activation
		// state, in case activation fails and they need to revert to "none"
		return nil, auth.ErrNotActivated
	}

	// Check if the cluster is in a partially-activated state. If so, allow it to
	// be completely deactivated so that it returns to normal
	var magicUserIsAdmin bool
	func() {
		a.adminMu.Lock()
		defer a.adminMu.Unlock()
		_, magicUserIsAdmin = a.adminCache[magicUser]
	}()
	if magicUserIsAdmin {
		_, err := col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
			a.admins.ReadWrite(stm).DeleteAll() // watchAdmins() will see the write
			return nil
		})
		if err != nil {
			return nil, err
		}
		return &auth.DeactivateResponse{}, nil
	}

	// Get calling user. The user must be a cluster admin to disable auth for the
	// cluster
	callerInfo, err := a.getAuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	isAdmin, err := a.isAdmin(ctx, callerInfo.Subject)
	if err != nil {
		return nil, err
	}
	if !isAdmin {
		return nil, &auth.ErrNotAuthorized{
			Subject: callerInfo.Subject,
			AdminOp: "DeactivateAuth",
		}
	}
	_, err = col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
		a.acls.ReadWrite(stm).DeleteAll()
		a.tokens.ReadWrite(stm).DeleteAll()
		a.admins.ReadWrite(stm).DeleteAll() // watchAdmins() will see the write
		a.members.ReadWrite(stm).DeleteAll()
		a.groups.ReadWrite(stm).DeleteAll()
		a.authConfig.ReadWrite(stm).DeleteAll()
		return nil
	})
	if err != nil {
		return nil, err
	}

	// wait until watchAdmins has deactivated auth, so that Deactivate() is less
	// likely to race with subsequent calls that expect auth to be deactivated.
	// TODO this is a bit hacky (checking repeatedly in a spin loop) but
	// Deactivate() is rarely called, and it helps avoid races due to other pachd
	// pods being out of date.
	if err := backoff.Retry(func() error {
		if a.activationState() != none {
			return fmt.Errorf("auth still activated")
		}
		return nil
	}, backoff.RetryEvery(time.Second)); err != nil {
		return nil, err
	}
	time.Sleep(time.Second) // give other pachd nodes time to update their cache
	return &auth.DeactivateResponse{}, nil
}

// GitHubTokenToUsername takes a OAuth access token issued by GitHub and uses
// it discover the username of the user who obtained the code (or verify that
// the code belongs to githubUsername). This is how Pachyderm currently
// implements authorization in a production cluster
func GitHubTokenToUsername(ctx context.Context, oauthToken string) (string, error) {
	if !githubTokenRegex.MatchString(oauthToken) && os.Getenv(DisableAuthenticationEnvVar) == "true" {
		logrus.Warnf("Pachyderm is deployed in DEV mode. The provided auth token "+
			"will NOT be verified with GitHub; the caller is automatically "+
			"authenticated as the GitHub user \"%s\"", oauthToken)
		return auth.GitHubPrefix + oauthToken, nil
	}

	// Initialize GitHub client with 'oauthToken'
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{
			AccessToken: oauthToken,
		},
	)
	tc := oauth2.NewClient(ctx, ts)
	gclient := github.NewClient(tc)

	// Retrieve the caller's GitHub Username (the empty string gets us the
	// authenticated user)
	user, _, err := gclient.Users.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("error getting the authenticated user: %v", err)
	}
	verifiedUsername := user.GetLogin()
	return auth.GitHubPrefix + verifiedUsername, nil
}

// GetAdmins implements the protobuf auth.GetAdmins RPC
func (a *apiServer) GetAdmins(ctx context.Context, req *auth.GetAdminsRequest) (resp *auth.GetAdminsResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.LogResp(req, resp, retErr, time.Since(start)) }(time.Now())
	switch a.activationState() {
	case none:
		return nil, auth.ErrNotActivated
	case full:
		// Get calling user. There is no auth check to see the list of cluster
		// admins, other than that the user must log in. Otherwise how will users
		// know who to ask for admin privileges? Requiring the user to be logged in
		// mitigates phishing
		_, err := a.getAuthenticatedUser(ctx)
		if err != nil {
			return nil, err
		}
	}

	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	resp = &auth.GetAdminsResponse{
		Admins: make([]string, 0, len(a.adminCache)),
	}
	for admin := range a.adminCache {
		resp.Admins = append(resp.Admins, admin)
	}
	return resp, nil
}

func (a *apiServer) validateModifyAdminsRequest(add []string, remove []string) error {
	// Check to make sure that req doesn't remove all cluster admins
	m := make(map[string]struct{})
	// copy existing admins into m
	func() {
		a.adminMu.Lock()
		defer a.adminMu.Unlock()
		for u := range a.adminCache {
			m[u] = struct{}{}
		}
	}()
	for _, u := range add {
		m[u] = struct{}{}
	}
	for _, u := range remove {
		delete(m, u)
	}

	// Confirm that there will be at least one admin.
	//
	// This is required so that the admin can get the cluster out of any broken
	// state that it may enter.
	if len(m) == 0 {
		return fmt.Errorf("invalid request: cannot remove all cluster administrators while auth is active, to avoid unfixable cluster states")
	}
	return nil
}

// ModifyAdmins implements the protobuf auth.ModifyAdmins RPC
func (a *apiServer) ModifyAdmins(ctx context.Context, req *auth.ModifyAdminsRequest) (resp *auth.ModifyAdminsResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.LogResp(req, resp, retErr, time.Since(start)) }(time.Now())
	switch a.activationState() {
	case none:
		return nil, auth.ErrNotActivated
	case partial:
		return nil, auth.ErrPartiallyActivated
	}

	// Get calling user. The user must be an admin to change the list of admins
	callerInfo, err := a.getAuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	isAdmin, err := a.isAdmin(ctx, callerInfo.Subject)
	if err != nil {
		return nil, err
	}
	if !isAdmin {
		return nil, &auth.ErrNotAuthorized{
			Subject: callerInfo.Subject,
			AdminOp: "ModifyAdmins",
		}
	}

	// Canonicalize GitHub usernames in request (must canonicalize before we can
	// validate, so we know who is actually being added/removed & can confirm
	// that not all admins are being removed)
	eg := &errgroup.Group{}
	canonicalizedToAdd := make([]string, len(req.Add))
	for i, user := range req.Add {
		i, user := i, user
		eg.Go(func() error {
			user, err = a.canonicalizeSubject(ctx, user)
			if err != nil {
				return err
			}
			canonicalizedToAdd[i] = user
			return nil
		})
	}
	canonicalizedToRemove := make([]string, len(req.Remove))
	for i, user := range req.Remove {
		i, user := i, user
		eg.Go(func() error {
			user, err = a.canonicalizeSubject(ctx, user)
			if err != nil {
				return err
			}
			canonicalizedToRemove[i] = user
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	err = a.validateModifyAdminsRequest(canonicalizedToAdd, canonicalizedToRemove)
	if err != nil {
		return nil, err
	}

	// Update "admins" list (watchAdmins() will update admins cache)
	for _, user := range canonicalizedToAdd {
		if _, err = col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
			return a.admins.ReadWrite(stm).Put(user, epsilon)
		}); err != nil && retErr == nil {
			retErr = err
		}
	}
	for _, user := range canonicalizedToRemove {
		if _, err = col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
			return a.admins.ReadWrite(stm).Delete(user)
		}); err != nil && retErr == nil {
			retErr = err
		}
	}
	if retErr != nil {
		return nil, retErr
	}
	return &auth.ModifyAdminsResponse{}, nil
}

// expiredClusterAdminCheck enforces that if the cluster's enterprise token is
// expired, only admins may log in.
func (a *apiServer) expiredClusterAdminCheck(ctx context.Context, username string) error {
	state, err := a.getEnterpriseTokenState()
	if err != nil {
		return fmt.Errorf("error confirming Pachyderm Enterprise token: %v", err)
	}

	isAdmin, err := a.isAdmin(ctx, username)
	if err != nil {
		return err
	}
	if state != enterpriseclient.State_ACTIVE && !isAdmin {
		return errors.New("Pachyderm Enterprise is not active in this " +
			"cluster (until Pachyderm Enterprise is re-activated or Pachyderm " +
			"auth is deactivated, only cluster admins can perform any operations)")
	}
	return nil
}

// Authenticate implements the protobuf auth.Authenticate RPC
func (a *apiServer) Authenticate(ctx context.Context, req *auth.AuthenticateRequest) (resp *auth.AuthenticateResponse, retErr error) {
	switch a.activationState() {
	case none:
		// PPS is authenticated by a token read from etcd. It never calls or needs
		// to call authenticate, even while the cluster is partway through the
		// activation process
		return nil, auth.ErrNotActivated
	case partial:
		return nil, auth.ErrPartiallyActivated
	}

	// We don't want to actually log the request/response since they contain
	// credentials.
	defer func(start time.Time) { a.LogResp(nil, nil, retErr, time.Since(start)) }(time.Now())

	// verify whatever credential the user has presented, and write a new
	// Pachyderm token for the user that their credential belongs to
	var pachToken string
	switch {
	case req.GitHubToken != "":
		// Determine caller's Pachyderm/GitHub username
		username, err := GitHubTokenToUsername(ctx, req.GitHubToken)
		if err != nil {
			return nil, err
		}

		// If the cluster's enterprise token is expired, only admins may log in
		if err := a.expiredClusterAdminCheck(ctx, username); err != nil {
			return nil, err
		}

		// Generate a new Pachyderm token and write it
		pachToken = uuid.NewWithoutDashes()
		if _, err := col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
			tokens := a.tokens.ReadWrite(stm)
			return tokens.PutTTL(hashToken(pachToken),
				&auth.TokenInfo{
					Subject: username,
					Source:  auth.TokenInfo_AUTHENTICATE,
				},
				defaultTokenTTLSecs)
		}); err != nil {
			return nil, fmt.Errorf("error storing auth token for user \"%s\": %v", username, err)
		}

	case req.OneTimePassword != "":
		if _, err := col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
			// read short-lived authentication code (and delete it if found)
			codes := a.authenticationCodes.ReadWrite(stm)
			key := hashToken(req.OneTimePassword)
			var otpInfo auth.OTPInfo
			if err := codes.Get(key, &otpInfo); err != nil {
				return err
			}
			codes.Delete(key)

			// If the cluster's enterprise token is expired, only admins may log in
			if err := a.expiredClusterAdminCheck(ctx, otpInfo.Subject); err != nil {
				return err
			}

			// Determine new token's TTL
			ttl := int64(defaultTokenTTLSecs)
			if otpInfo.SessionExpiration != nil {
				expiration, err := types.TimestampFromProto(otpInfo.SessionExpiration)
				if err != nil {
					return fmt.Errorf("invalid timestamp in OTPInfo, could not " +
						"authenticate (try obtaining a new OTP)")
				}
				if !expiration.IsZero() {
					// divide instead of calling Seconds() to avoid float-based rounding
					// errors
					newTTL := int64(expiration.Sub(time.Now()) / time.Second)
					if newTTL < ttl {
						ttl = newTTL
					}
				}
			}

			// write long-lived pachyderm token
			pachToken = uuid.NewWithoutDashes()
			return a.tokens.ReadWrite(stm).PutTTL(hashToken(pachToken), &auth.TokenInfo{
				Subject: otpInfo.Subject,
				Source:  auth.TokenInfo_AUTHENTICATE,
			}, ttl)
		}); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("unrecognized authentication mechanism (old pachd?)")
	}

	// Return new pachyderm token to caller
	return &auth.AuthenticateResponse{
		PachToken: pachToken,
	}, nil
}

func (a *apiServer) getCallerTTL(ctx context.Context) (int64, error) {
	token, err := getAuthToken(ctx)
	if err != nil {
		return 0, err
	}
	ttl, err := a.tokens.ReadOnly(ctx).TTL(hashToken(token)) // lookup token TTL
	if err != nil {
		return 0, fmt.Errorf("error looking up TTL for token: %v", err)
	}
	return ttl, nil
}

// GetOneTimePassword implements the protobuf auth.GetOneTimePassword RPC
func (a *apiServer) GetOneTimePassword(ctx context.Context, req *auth.GetOneTimePasswordRequest) (resp *auth.GetOneTimePasswordResponse, retErr error) {
	// We don't want to actually log the request/response since they contain
	// credentials.
	defer func(start time.Time) { a.LogResp(nil, nil, retErr, time.Since(start)) }(time.Now())

	// Make sure auth is activated
	switch a.activationState() {
	case none:
		// PPS is authenticated by a token read from etcd. It never calls or needs
		// to call authenticate, even while the cluster is partway through the
		// activation process
		return nil, auth.ErrNotActivated
	case partial:
		return nil, auth.ErrPartiallyActivated
	}
	if req.Subject == magicUser {
		return nil, fmt.Errorf("GetAuthTokenRequest.Subject is invalid")
	}

	// Get current caller and check if they're authorized if req.Subject is set
	// to a different user
	callerInfo, err := a.getAuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	isAdmin, err := a.isAdmin(ctx, callerInfo.Subject)
	if err != nil {
		return nil, err
	}

	// Canonicalize req.Subject, authorize caller
	if req.Subject == "" {
		// [Simple case] caller wants an implicit OTP for themselves
		// Canonicalization: callerInfo.Subject is already canonical.
		// Authorization: Getting an OTP for yourself is always authorized
		// TTL: Use caller's current token if non-admin, default OTP TTL otherwise
		// NOTE: After this point, req.Subject may be magicUser (even though we
		// reject magicUser when set explicitly
		req.Subject = callerInfo.Subject
	} else {
		// [Harder case] explicit req.Subject
		// canonicalize req.Subject
		subject, err := a.canonicalizeSubject(ctx, req.Subject)
		if err != nil {
			return nil, err
		}
		req.Subject = subject

		// Authorization: caller must be admin to get OTP for another user
		if !isAdmin && req.Subject != callerInfo.Subject {
			return nil, &auth.ErrNotAuthorized{
				Subject: callerInfo.Subject,
				AdminOp: "GetOneTimePassword on behalf of another user",
			}
		}
	}

	// Compute TTL for new token that the user will get once OTP is exchanged
	// Note: Admins always use default TTL (30 days currently), which means they
	// can extend their session by getting an OTP and exchanging it for a new
	// token with a later expiration than their curren token
	var ttl = int64(defaultTokenTTLSecs)
	if !isAdmin {
		// Caller is getting OTP for themselves--use TTL of their current token
		ttl, err = a.getCallerTTL(ctx)
		if err != nil {
			return nil, err
		}
	}

	// Convert TTL to expiration time
	// Note: ttl <= 0 means that the user's session will not expire (e.g. because
	// they are a robot user)
	var expiration time.Time
	if ttl > 0 {
		if ttl <= 10 {
			// session is too short to be meaningful -- return an error
			return nil, fmt.Errorf("session expires too soon to get an " +
				"authentication code")
		}
		expiration = time.Now().Add(time.Duration(ttl-1) * time.Second)
	}

	// Generate authentication code with same (or slightly shorter) expiration
	code, err := a.getOneTimePassword(ctx, req.Subject, expiration)
	if err != nil {
		return nil, err
	}
	return &auth.GetOneTimePasswordResponse{
		Code: code,
	}, nil
}

// getOneTimePassword contains the implementation of GetOneTimePassword,
// but is also called directly by handleSAMLREsponse. It generates a
// short-lived authentication code for 'username', writes it to
// a.authenticationCodes, and returns it
func (a *apiServer) getOneTimePassword(ctx context.Context, username string, expiration time.Time) (code string, err error) {
	// Create OTPInfo that will be stored
	otpInfo := &auth.OTPInfo{
		Subject: username,
	}
	if !expiration.IsZero() {
		expirationProto, err := types.TimestampProto(expiration)
		if err != nil {
			return "", fmt.Errorf("could not create OTP with expiration time %s: %v",
				expiration.String(), err)
		}
		otpInfo.SessionExpiration = expirationProto
	}

	// Generate and store new OTP
	code = "auth_code:" + uuid.NewWithoutDashes()
	if _, err = col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
		return a.authenticationCodes.ReadWrite(stm).PutTTL(hashToken(code),
			otpInfo, defaultAuthCodeTTLSecs)
	}); err != nil {
		return "", err
	}
	return code, nil
}

// AuthorizeInTransaction is identical to Authorize except that it can run
// inside an existing etcd STM transaction.  This is not an RPC.
func (a *apiServer) AuthorizeInTransaction(
	txnCtx *txnenv.TransactionContext,
	req *auth.AuthorizeRequest,
) (resp *auth.AuthorizeResponse, retErr error) {
	if a.activationState() == none {
		return nil, auth.ErrNotActivated
	}

	callerInfo, err := a.getAuthenticatedUser(txnCtx.ClientContext)
	if err != nil {
		return nil, err
	}
	isAdmin, err := a.isAdmin(txnCtx.ClientContext, callerInfo.Subject)
	if err != nil {
		return nil, err
	}

	// admins are always authorized
	if isAdmin {
		return &auth.AuthorizeResponse{Authorized: true}, nil
	}

	if req.Repo == ppsconsts.SpecRepo {
		// All users are authorized to read from the spec repo, but only admins are
		// authorized to write to it (writing to it may break your cluster)
		return &auth.AuthorizeResponse{
			Authorized: req.Scope == auth.Scope_READER,
		}, nil
	}

	// If the cluster's enterprise token is expired, only admins and pipelines may
	// authorize (and admins are already handled)
	state, err := a.getEnterpriseTokenState()
	if err != nil {
		return nil, fmt.Errorf("error confirming Pachyderm Enterprise token: %v", err)
	}
	if state != enterpriseclient.State_ACTIVE &&
		!strings.HasPrefix(callerInfo.Subject, auth.PipelinePrefix) {
		return nil, errors.New("Pachyderm Enterprise is not active in this " +
			"cluster (until Pachyderm Enterprise is re-activated or Pachyderm " +
			"auth is deactivated, only cluster admins can perform any operations)")
	}

	// Get ACL to check
	var acl auth.ACL
	if err := a.acls.ReadWrite(txnCtx.Stm).Get(req.Repo, &acl); err != nil && !col.IsErrNotFound(err) {
		return nil, fmt.Errorf("error getting ACL for repo \"%s\": %v", req.Repo, err)
	}

	scope, err := a.getScope(txnCtx.ClientContext, callerInfo.Subject, &acl)
	if err != nil {
		return nil, err
	}
	return &auth.AuthorizeResponse{
		Authorized: scope >= req.Scope,
	}, nil
}

// Authorize implements the protobuf auth.Authorize RPC
func (a *apiServer) Authorize(
	ctx context.Context,
	req *auth.AuthorizeRequest,
) (resp *auth.AuthorizeResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.LogResp(req, resp, retErr, time.Since(start)) }(time.Now())

	var response *auth.AuthorizeResponse
	if err := a.txnEnv.WithReadContext(ctx, func(txnCtx *txnenv.TransactionContext) error {
		var err error
		response, err = a.AuthorizeInTransaction(txnCtx, req)
		return err
	}); err != nil {
		return nil, err
	}
	return response, nil
}

// WhoAmI implements the protobuf auth.WhoAmI RPC
func (a *apiServer) WhoAmI(ctx context.Context, req *auth.WhoAmIRequest) (resp *auth.WhoAmIResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.LogResp(req, resp, retErr, time.Since(start)) }(time.Now())
	if a.activationState() == none {
		return nil, auth.ErrNotActivated
	}

	callerInfo, err := a.getAuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	isAdmin, err := a.isAdmin(ctx, callerInfo.Subject)
	if err != nil {
		return nil, err
	}

	// Get TTL of user's token
	ttl := int64(-1) // value returned by etcd for keys w/ no lease (no TTL)
	if callerInfo.Subject != magicUser {
		token, err := getAuthToken(ctx)
		if err != nil {
			return nil, err
		}
		ttl, err = a.tokens.ReadOnly(ctx).TTL(hashToken(token)) // lookup token TTL
		if err != nil {
			return nil, fmt.Errorf("error looking up TTL for token: %v", err)
		}
	}

	// return final result
	return &auth.WhoAmIResponse{
		Username: callerInfo.Subject,
		IsAdmin:  isAdmin,
		TTL:      ttl,
	}, nil
}

func validateSetScopeRequest(ctx context.Context, req *auth.SetScopeRequest) error {
	if req.Username == "" {
		return fmt.Errorf("invalid request: must set username")
	}
	if req.Repo == "" {
		return fmt.Errorf("invalid request: must set repo")
	}
	return nil
}

func (a *apiServer) isAdmin(ctx context.Context, subject string) (bool, error) {
	if subject == magicUser {
		return true, nil
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	if _, ok := a.adminCache[subject]; ok {
		return true, nil
	}

	// Get scope based on group access
	groups, err := a.getGroups(ctx, subject)
	if err != nil {
		return false, fmt.Errorf("could not retrieve caller's group memberships: %v", err)
	}
	for _, g := range groups {
		if _, ok := a.adminCache[g]; ok {
			return true, nil
		}
	}
	return false, nil
}

// SetScopeInTransaction is identical to SetScope except that it can run inside
// an existing etcd STM transaction.  This is not an RPC.
func (a *apiServer) SetScopeInTransaction(
	txnCtx *txnenv.TransactionContext,
	req *auth.SetScopeRequest,
) (*auth.SetScopeResponse, error) {
	if a.activationState() == none {
		return nil, auth.ErrNotActivated
	}

	// validate request & authenticate user
	if err := validateSetScopeRequest(txnCtx.ClientContext, req); err != nil {
		return nil, err
	}
	callerInfo, err := a.getAuthenticatedUser(txnCtx.ClientContext)
	if err != nil {
		return nil, err
	}
	isAdmin, err := a.isAdmin(txnCtx.ClientContext, callerInfo.Subject)
	if err != nil {
		return nil, err
	}

	acls := a.acls.ReadWrite(txnCtx.Stm)
	var acl auth.ACL
	if err := acls.Get(req.Repo, &acl); err != nil {
		if !col.IsErrNotFound(err) {
			return nil, err
		}
		// ACL not found. Check that repo exists (return error if not).
		_, err = txnCtx.Pfs().InspectRepoInTransaction(
			txnCtx,
			&pfs.InspectRepoRequest{Repo: &pfs.Repo{Name: req.Repo}},
		)
		if err != nil {
			return nil, err
		}

		// Repo exists, but has no ACL. Create default (empty) ACL
		acl.Entries = make(map[string]auth.Scope)
	}

	// Check if the caller is authorized
	authorized, err := func() (bool, error) {
		if isAdmin {
			// admins are automatically authorized
			return true, nil
		}

		// Check if the cluster's enterprise token is expired (fail if so)
		state, err := a.getEnterpriseTokenState()
		if err != nil {
			return false, fmt.Errorf("error confirming Pachyderm Enterprise token: %v", err)
		}
		if state != enterpriseclient.State_ACTIVE {
			return false, fmt.Errorf("Pachyderm Enterprise is not active in this " +
				"cluster (only a cluster admin can set a scope)")
		}

		// Check if the user or one of their groups is on the ACL directly
		scope, err := a.getScope(txnCtx.ClientContext, callerInfo.Subject, &acl)
		if err != nil {
			return false, err
		}
		if scope == auth.Scope_OWNER {
			return true, nil
		}
		return false, nil
	}()
	if err != nil {
		return nil, err
	}
	if !authorized {
		return nil, &auth.ErrNotAuthorized{
			Subject:  callerInfo.Subject,
			Repo:     req.Repo,
			Required: auth.Scope_OWNER,
		}
	}

	// Scope change is authorized. Make the change
	principal, err := a.canonicalizeSubject(txnCtx.ClientContext, req.Username)
	if err != nil {
		return nil, err
	}
	if req.Scope != auth.Scope_NONE {
		acl.Entries[principal] = req.Scope
	} else {
		delete(acl.Entries, principal)
	}
	if len(acl.Entries) == 0 {
		err = acls.Delete(req.Repo)
	} else {
		err = acls.Put(req.Repo, &acl)
	}
	if err != nil {
		return nil, err
	}
	return &auth.SetScopeResponse{}, nil
}

// SetScope implements the protobuf auth.SetScope RPC
func (a *apiServer) SetScope(ctx context.Context, req *auth.SetScopeRequest) (resp *auth.SetScopeResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.LogResp(req, resp, retErr, time.Since(start)) }(time.Now())

	var response *auth.SetScopeResponse
	if err := a.txnEnv.WithTransaction(ctx, func(txn txnenv.Transaction) error {
		var err error
		response, err = txn.SetScope(req)
		return err
	}); err != nil {
		return nil, err
	}
	return response, nil
}

// getScope is a helper function for the GetScope GRPC API, as well is
// Authorized() and other authorization checks (e.g. checking if a user is an
// OWNER to determine if they can modify an ACL).
func (a *apiServer) getScope(ctx context.Context, subject string, acl *auth.ACL) (auth.Scope, error) {
	// Get scope based on user's direct access
	scope := acl.Entries[subject]

	// Expand scope based on group access
	groups, err := a.getGroups(ctx, subject)
	if err != nil {
		return auth.Scope_NONE, fmt.Errorf("could not retrieve caller's "+
			"group memberships: %v", err)
	}
	for _, g := range groups {
		groupScope := acl.Entries[g]
		if scope < groupScope {
			scope = groupScope
		}
	}
	return scope, nil
}

// GetScopeInTransaction is identical to GetScope except that it can run inside
// an existing etcd STM transaction.  This is not an RPC.
func (a *apiServer) GetScopeInTransaction(
	txnCtx *txnenv.TransactionContext,
	req *auth.GetScopeRequest,
) (*auth.GetScopeResponse, error) {
	if a.activationState() == none {
		return nil, auth.ErrNotActivated
	}

	callerInfo, err := a.getAuthenticatedUser(txnCtx.ClientContext)
	if err != nil {
		return nil, err
	}
	callerIsAdmin, err := a.isAdmin(txnCtx.ClientContext, callerInfo.Subject)
	if err != nil {
		return nil, err
	}

	// Check if the cluster's enterprise token is expired (fail if so)
	// Note: this is duplicated from a.expiredClusterAdminCheck, but we need the
	// admin information elsewhere, so the code is copied here
	state, err := a.getEnterpriseTokenState()
	if err != nil {
		return nil, fmt.Errorf("error confirming Pachyderm Enterprise token: %v", err)
	}
	if state != enterpriseclient.State_ACTIVE && !callerIsAdmin {
		return nil, errors.New("Pachyderm Enterprise is not active in this " +
			"cluster (until Pachyderm Enterprise is re-activated or Pachyderm " +
			"auth is deactivated, only cluster admins can perform any operations)")
	}

	// If the caller is getting another user's scopes, the caller must have
	// READER access to every repo in the request--check this requirement
	targetSubject := callerInfo.Subject
	mustHaveReadAccess := false
	if req.Username != "" {
		targetSubject, err = a.canonicalizeSubject(txnCtx.ClientContext, req.Username)
		if err != nil {
			return nil, err
		}
		mustHaveReadAccess = true
	}

	// Read repo ACLs from etcd & compute final result
	// Note: for now, we don't return OWNER if the user is an admin, even though
	// that's their effective access scope for all repos--the caller may want to
	// know what will happen if the user's admin privileges are revoked. Note
	// that pfs.ListRepo overrides this logic—the auth info it returns for a
	// listed repo indicates that a user is OWNER of all repos if they are an
	// admin
	acls := a.acls.ReadWrite(txnCtx.Stm)
	response := new(auth.GetScopeResponse)
	for _, repo := range req.Repos {
		var acl auth.ACL
		if err := acls.Get(repo, &acl); err != nil && !col.IsErrNotFound(err) {
			return nil, err
		}

		// determine if ACL read is authorized
		if mustHaveReadAccess && !callerIsAdmin {
			// Caller is getting another user's scopes. Check if the caller is
			// authorized to view this repo's ACL
			callerScope, err := a.getScope(txnCtx.ClientContext, callerInfo.Subject, &acl)
			if err != nil {
				return nil, err
			}
			if callerScope < auth.Scope_READER {
				return nil, &auth.ErrNotAuthorized{
					Subject:  callerInfo.Subject,
					Repo:     repo,
					Required: auth.Scope_READER,
				}
			}
		}

		// compute target's access scope to this repo
		targetScope, err := a.getScope(txnCtx.ClientContext, targetSubject, &acl)
		if err != nil {
			return nil, err
		}
		response.Scopes = append(response.Scopes, targetScope)
	}

	return response, nil
}

// GetScope implements the protobuf auth.GetScope RPC
func (a *apiServer) GetScope(ctx context.Context, req *auth.GetScopeRequest) (resp *auth.GetScopeResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.LogResp(req, resp, retErr, time.Since(start)) }(time.Now())

	var response *auth.GetScopeResponse
	if err := a.txnEnv.WithReadContext(ctx, func(txnCtx *txnenv.TransactionContext) error {
		var err error
		response, err = a.GetScopeInTransaction(txnCtx, req)
		return err
	}); err != nil {
		return nil, err
	}
	return response, nil
}

// GetACLInTransaction is identical to GetACL except that it can run inside
// an existing etcd STM transaction.  This is not an RPC.
func (a *apiServer) GetACLInTransaction(
	txnCtx *txnenv.TransactionContext,
	req *auth.GetACLRequest,
) (*auth.GetACLResponse, error) {
	if a.activationState() == none {
		return nil, auth.ErrNotActivated
	}

	// Validate request
	if req.Repo == "" {
		return nil, fmt.Errorf("invalid request: must provide name of repo to get that repo's ACL")
	}

	// Get calling user
	callerInfo, err := a.getAuthenticatedUser(txnCtx.ClientContext)
	if err != nil {
		return nil, err
	}
	if err := a.expiredClusterAdminCheck(txnCtx.ClientContext, callerInfo.Subject); err != nil {
		return nil, err
	}

	// Read repo ACL from etcd
	acl := &auth.ACL{}
	if err = a.acls.ReadWrite(txnCtx.Stm).Get(req.Repo, acl); err != nil && !col.IsErrNotFound(err) {
		return nil, err
	}
	response := &auth.GetACLResponse{
		Entries: make([]*auth.ACLEntry, 0),
	}
	for user, scope := range acl.Entries {
		response.Entries = append(response.Entries, &auth.ACLEntry{
			Username: user,
			Scope:    scope,
		})
	}
	// For now, no access is require to read a repo's ACL
	// https://github.com/pachyderm/pachyderm/issues/2353
	return response, nil
}

// GetACL implements the protobuf auth.GetACL RPC
func (a *apiServer) GetACL(ctx context.Context, req *auth.GetACLRequest) (resp *auth.GetACLResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.LogResp(req, resp, retErr, time.Since(start)) }(time.Now())

	var response *auth.GetACLResponse
	if err := a.txnEnv.WithReadContext(ctx, func(txnCtx *txnenv.TransactionContext) error {
		var err error
		response, err = a.GetACLInTransaction(txnCtx, req)
		return err
	}); err != nil {
		return nil, err
	}
	return response, nil
}

// SetACLInTransaction is identical to SetACL except that it can run inside
// an existing etcd STM transaction.  This is not an RPC.
func (a *apiServer) SetACLInTransaction(
	txnCtx *txnenv.TransactionContext,
	req *auth.SetACLRequest,
) (*auth.SetACLResponse, error) {
	if a.activationState() == none {
		return nil, auth.ErrNotActivated
	}

	// Validate request
	if req.Repo == "" {
		return nil, fmt.Errorf("invalid request: must provide name of repo you want to modify")
	}

	// Get calling user
	callerInfo, err := a.getAuthenticatedUser(txnCtx.ClientContext)
	if err != nil {
		return nil, err
	}
	isAdmin, err := a.isAdmin(txnCtx.ClientContext, callerInfo.Subject)
	if err != nil {
		return nil, err
	}

	// Canonicalize entries in the request (must have canonical request before we
	// can authorize, as we inspect the ACL contents during authorization in the
	// case where we're creating a new repo)
	eg := &errgroup.Group{}
	var aclMu sync.Mutex
	newACL := new(auth.ACL)
	if len(req.Entries) > 0 {
		newACL.Entries = make(map[string]auth.Scope)
	}
	for _, entry := range req.Entries {
		user, scope := entry.Username, entry.Scope
		if user == magicUser {
			continue
		}
		eg.Go(func() error {
			principal, err := a.canonicalizeSubject(txnCtx.ClientContext, user)
			if err != nil {
				return err
			}
			aclMu.Lock()
			defer aclMu.Unlock()
			newACL.Entries[principal] = scope
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	// Read repo ACL from etcd
	acls := a.acls.ReadWrite(txnCtx.Stm)

	// determine if the caller is authorized to set this repo's ACL
	authorized, err := func() (bool, error) {
		if isAdmin {
			// admins are automatically authorized
			return true, nil
		}

		// Check if the cluster's enterprise token is expired (fail if so)
		state, err := a.getEnterpriseTokenState()
		if err != nil {
			return false, fmt.Errorf("error confirming Pachyderm Enterprise token: %v", err)
		}
		if state != enterpriseclient.State_ACTIVE {
			return false, fmt.Errorf("Pachyderm Enterprise is not active in this " +
				"cluster (only a cluster admin can modify an ACL)")
		}

		// Check if there is an existing ACL, and if the user is on it
		var acl auth.ACL
		if err := acls.Get(req.Repo, &acl); err != nil {
			// ACL not found -- construct empty ACL proto
			acl.Entries = make(map[string]auth.Scope)
		}
		if len(acl.Entries) > 0 {
			// ACL is present; caller must be authorized directly
			scope, err := a.getScope(txnCtx.ClientContext, callerInfo.Subject, &acl)
			if err != nil {
				return false, err
			}
			if scope == auth.Scope_OWNER {
				return true, nil
			}
			return false, nil
		}

		// No ACL -- check if the repo being modified exists
		_, err = txnCtx.Pfs().InspectRepoInTransaction(txnCtx, &pfs.InspectRepoRequest{Repo: &pfs.Repo{Name: req.Repo}})
		if err == nil {
			// Repo exists -- user isn't authorized
			return false, nil
		} else if !strings.HasSuffix(err.Error(), "not found") {
			// Unclear if repo exists -- return error
			return false, fmt.Errorf("could not inspect \"%s\": %v", req.Repo, err)
		} else if len(newACL.Entries) == 1 &&
			newACL.Entries[callerInfo.Subject] == auth.Scope_OWNER {
			// Special case: Repo doesn't exist, but user is creating a new Repo, and
			// making themself the owner, e.g. for CreateRepo or CreatePipeline, then
			// the request is authorized
			return true, nil
		}
		return false, err
	}()
	if err != nil {
		return nil, err
	}
	if !authorized {
		return nil, &auth.ErrNotAuthorized{
			Subject:  callerInfo.Subject,
			Repo:     req.Repo,
			Required: auth.Scope_OWNER,
		}
	}

	// Set new ACL
	if len(newACL.Entries) == 0 {
		err := acls.Delete(req.Repo)
		if err != nil && !col.IsErrNotFound(err) {
			return nil, err
		}
	} else {
		err = acls.Put(req.Repo, newACL)
		if err != nil {
			return nil, fmt.Errorf("could not put new ACL: %v", err)
		}
	}
	return &auth.SetACLResponse{}, nil
}

// SetACL implements the protobuf auth.SetACL RPC
func (a *apiServer) SetACL(ctx context.Context, req *auth.SetACLRequest) (resp *auth.SetACLResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.LogResp(req, resp, retErr, time.Since(start)) }(time.Now())

	var response *auth.SetACLResponse
	if err := a.txnEnv.WithTransaction(ctx, func(txn txnenv.Transaction) error {
		var err error
		response, err = txn.SetACL(req)
		return err
	}); err != nil {
		return nil, err
	}
	return response, nil
}

// GetAuthToken implements the protobuf auth.GetAuthToken RPC
func (a *apiServer) GetAuthToken(ctx context.Context, req *auth.GetAuthTokenRequest) (resp *auth.GetAuthTokenResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.LogResp(req, resp, retErr, time.Since(start)) }(time.Now())
	if a.activationState() == none {
		return nil, auth.ErrNotActivated
	}
	if req.Subject == "" {
		return nil, fmt.Errorf("must set GetAuthTokenRequest.Subject")
	}
	if req.Subject == magicUser {
		return nil, fmt.Errorf("GetAuthTokenRequest.Subject is invalid")
	}

	// Get current caller and authorize them if req.Subject is set to a different
	// user
	callerInfo, err := a.getAuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	isAdmin, err := a.isAdmin(ctx, callerInfo.Subject)
	if err != nil {
		return nil, err
	}

	// TODO(msteffen): This code is duplicated here and in GetOneTimePassword.
	// These two RPC handlers should probably be unified
	// Canonicalize req.Subject, authorize caller
	if req.Subject == "" {
		// [Simple case] caller wants an implicit OTP for themselves
		// Canonicalization: callerInfo.Subject is already canonical.
		// Authorization: Getting an OTP for yourself is always authorized
		// TTL: Use caller's current token if non-admin, default OTP TTL otherwise
		// NOTE: After this point, req.Subject may be magicUser (even though we
		// reject magicUser when set explicitly
		req.Subject = callerInfo.Subject
	} else {
		// [Harder case] explicit req.Subject
		// canonicalize req.Subject
		req.Subject, err = a.canonicalizeSubject(ctx, req.Subject)
		if err != nil {
			return nil, err
		}

		// Authorization: caller must be admin to get OTP for another user
		if !isAdmin && req.Subject != callerInfo.Subject {
			return nil, &auth.ErrNotAuthorized{
				Subject: callerInfo.Subject,
				AdminOp: "GetOneTimePassword on behalf of another user",
			}
		}
	}

	// Compute TTL for new token that the user will get once OTP is exchanged
	// Note: Admins always use default TTL (30 days currently), which means they
	// can extend their session by getting an OTP and exchanging it for a new
	// token with a later expiration than their curren token
	var ttl = int64(defaultTokenTTLSecs)
	if !isAdmin {
		// Caller is getting OTP for themselves--use TTL of their current token
		ttl, err = a.getCallerTTL(ctx)
		if err != nil {
			return nil, err
		}
		if req.TTL == 0 || req.TTL > ttl {
			req.TTL = ttl
		}
	}
	tokenInfo := auth.TokenInfo{
		Source:  auth.TokenInfo_GET_TOKEN,
		Subject: req.Subject,
	}

	// generate new token, and write to etcd
	token := uuid.NewWithoutDashes()
	if _, err := col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
		return a.tokens.ReadWrite(stm).PutTTL(hashToken(token), &tokenInfo, req.TTL)
	}); err != nil {
		if tokenInfo.Subject != magicUser {
			return nil, fmt.Errorf("error storing token for user \"%s\": %v", tokenInfo.Subject, err)
		}
		return nil, fmt.Errorf("error storing token: %v", err)
	}
	return &auth.GetAuthTokenResponse{
		Subject: req.Subject,
		Token:   token,
	}, nil
}

// ExtendAuthToken implements the protobuf auth.ExtendAuthToken RPC
func (a *apiServer) ExtendAuthToken(ctx context.Context, req *auth.ExtendAuthTokenRequest) (resp *auth.ExtendAuthTokenResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.LogResp(req, resp, retErr, time.Since(start)) }(time.Now())
	if a.activationState() != full {
		return nil, auth.ErrNotActivated
	}
	if req.TTL == 0 {
		return nil, fmt.Errorf("invalid request: ExtendAuthTokenRequest.TTL must be > 0")
	}

	// Only admins can extend auth tokens (for now)
	callerInfo, err := a.getAuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	isAdmin, err := a.isAdmin(ctx, callerInfo.Subject)
	if err != nil {
		return nil, err
	}
	if !isAdmin {
		return nil, &auth.ErrNotAuthorized{
			Subject: callerInfo.Subject,
			AdminOp: "ExtendAuthToken",
		}
	}

	// Only let people extend tokens by up to 30 days (the equivalent of logging
	// in again)
	if req.TTL > defaultTokenTTLSecs {
		return nil, fmt.Errorf("can only extend tokens by at most %d seconds", defaultTokenTTLSecs)
	}

	// The token must already exist. If a token has been revoked, it can't be
	// extended
	if _, err := col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
		tokens := a.tokens.ReadWrite(stm)

		// Actually look up the request token in the relevant collections
		var tokenInfo auth.TokenInfo
		if err := tokens.Get(hashToken(req.Token), &tokenInfo); err != nil && !col.IsErrNotFound(err) {
			return err
		}
		if tokenInfo.Subject == "" {
			return auth.ErrBadToken
		}

		ttl, err := tokens.TTL(hashToken(req.Token))
		if err != nil {
			return fmt.Errorf("Error looking up TTL for token: %v", err)
		}
		// TODO(msteffen): ttl may be -1 if the token has no TTL. We deliberately do
		// not check this case so that admins can put TTLs on tokens that don't have
		// them (otherwise any attempt to do so would get ErrTooShortTTL), but that
		// decision may be revised
		if req.TTL < ttl {
			return auth.ErrTooShortTTL{
				RequestTTL:  req.TTL,
				ExistingTTL: ttl,
			}
		}
		return tokens.PutTTL(hashToken(req.Token), &tokenInfo, req.TTL)
	}); err != nil {
		return nil, err
	}
	return &auth.ExtendAuthTokenResponse{}, nil
}

// RevokeAuthToken implements the protobuf auth.RevokeAuthToken RPC
func (a *apiServer) RevokeAuthToken(ctx context.Context, req *auth.RevokeAuthTokenRequest) (resp *auth.RevokeAuthTokenResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.LogResp(req, resp, retErr, time.Since(start)) }(time.Now())
	if a.activationState() != full {
		return nil, auth.ErrNotActivated
	}

	// Get the caller. Users can revoke their own tokens, and admins can revoke
	// any user's token
	callerInfo, err := a.getAuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	isAdmin, err := a.isAdmin(ctx, callerInfo.Subject)
	if err != nil {
		return nil, err
	}

	if _, err := col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
		tokens := a.tokens.ReadWrite(stm)
		var tokenInfo auth.TokenInfo
		if err := tokens.Get(hashToken(req.Token), &tokenInfo); err != nil {
			if col.IsErrNotFound(err) {
				return nil
			}
			return err
		}
		if !isAdmin && tokenInfo.Subject != callerInfo.Subject {
			return &auth.ErrNotAuthorized{
				Subject: callerInfo.Subject,
				AdminOp: "RevokeAuthToken on another user's token",
			}
		}
		return tokens.Delete(hashToken(req.Token))
	}); err != nil {
		return nil, err
	}
	return &auth.RevokeAuthTokenResponse{}, nil
}

// setGroupsForUserInternal is a helper function used by SetGroupsForUser, and
// also by handleSAMLResponse (which updates group membership information based
// on signed SAML assertions). This does no auth checks, so the caller must do
// all relevant authorization.
func (a *apiServer) setGroupsForUserInternal(ctx context.Context, subject string, groups []string) error {
	_, err := col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
		members := a.members.ReadWrite(stm)

		// Get groups to remove/add user from/to
		var removeGroups auth.Groups
		addGroups := addToSet(nil, groups...)
		if err := members.Get(subject, &removeGroups); err == nil {
			for _, group := range groups {
				if removeGroups.Groups[group] {
					removeGroups.Groups = removeFromSet(removeGroups.Groups, group)
					addGroups = removeFromSet(addGroups, group)
				}
			}
		}

		// Set groups for user
		if err := members.Put(subject, &auth.Groups{
			Groups: addToSet(nil, groups...),
		}); err != nil {
			return err
		}

		// Remove user from previous groups
		groups := a.groups.ReadWrite(stm)
		var membersProto auth.Users
		for group := range removeGroups.Groups {
			if err := groups.Upsert(group, &membersProto, func() error {
				membersProto.Usernames = removeFromSet(membersProto.Usernames, subject)
				return nil
			}); err != nil {
				return err
			}
		}

		// Add user to new groups
		for group := range addGroups {
			if err := groups.Upsert(group, &membersProto, func() error {
				membersProto.Usernames = addToSet(membersProto.Usernames, subject)
				return nil
			}); err != nil {
				return err
			}
		}

		return nil
	})
	return err
}

// SetGroupsForUser implements the protobuf auth.SetGroupsForUser RPC
func (a *apiServer) SetGroupsForUser(ctx context.Context, req *auth.SetGroupsForUserRequest) (resp *auth.SetGroupsForUserResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.LogResp(req, resp, retErr, time.Since(start)) }(time.Now())
	if a.activationState() != full {
		return nil, auth.ErrNotActivated
	}

	callerInfo, err := a.getAuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	isAdmin, err := a.isAdmin(ctx, callerInfo.Subject)
	if err != nil {
		return nil, err
	}

	if !isAdmin {
		return nil, &auth.ErrNotAuthorized{
			Subject: callerInfo.Subject,
			AdminOp: "SetGroupsForUser",
		}
	}

	subject, err := a.canonicalizeSubject(ctx, req.Username)
	if err != nil {
		return nil, err
	}
	// TODO(msteffen): canonicalize group names
	if err := a.setGroupsForUserInternal(ctx, subject, req.Groups); err != nil {
		return nil, err
	}
	return &auth.SetGroupsForUserResponse{}, nil
}

// ModifyMembers implements the protobuf auth.ModifyMembers RPC
func (a *apiServer) ModifyMembers(ctx context.Context, req *auth.ModifyMembersRequest) (resp *auth.ModifyMembersResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.LogResp(req, resp, retErr, time.Since(start)) }(time.Now())
	if a.activationState() != full {
		return nil, auth.ErrNotActivated
	}

	callerInfo, err := a.getAuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	isAdmin, err := a.isAdmin(ctx, callerInfo.Subject)
	if err != nil {
		return nil, err
	}

	if !isAdmin {
		return nil, &auth.ErrNotAuthorized{
			Subject: callerInfo.Subject,
			AdminOp: "ModifyMembers",
		}
	}

	add, err := a.canonicalizeSubjects(ctx, req.Add)
	if err != nil {
		return nil, err
	}
	// TODO(bryce) Skip canonicalization if the users can be found.
	remove, err := a.canonicalizeSubjects(ctx, req.Remove)
	if err != nil {
		return nil, err
	}

	if _, err := col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
		members := a.members.ReadWrite(stm)
		var groupsProto auth.Groups
		for _, username := range add {
			if err := members.Upsert(username, &groupsProto, func() error {
				groupsProto.Groups = addToSet(groupsProto.Groups, req.Group)
				return nil
			}); err != nil {
				return err
			}
		}
		for _, username := range remove {
			if err := members.Upsert(username, &groupsProto, func() error {
				groupsProto.Groups = removeFromSet(groupsProto.Groups, req.Group)
				return nil
			}); err != nil {
				return err
			}
		}

		groups := a.groups.ReadWrite(stm)
		var membersProto auth.Users
		if err := groups.Upsert(req.Group, &membersProto, func() error {
			membersProto.Usernames = addToSet(membersProto.Usernames, add...)
			membersProto.Usernames = removeFromSet(membersProto.Usernames, remove...)
			return nil
		}); err != nil {
			return err
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return &auth.ModifyMembersResponse{}, nil
}

func addToSet(set map[string]bool, elems ...string) map[string]bool {
	if set == nil {
		set = map[string]bool{}
	}

	for _, elem := range elems {
		set[elem] = true
	}
	return set
}

func removeFromSet(set map[string]bool, elems ...string) map[string]bool {
	if set != nil {
		for _, elem := range elems {
			delete(set, elem)
		}
	}

	return set
}

// getGroups is a helper function used primarily by the GRPC API GetGroups, but
// also by Authorize() and isAdmin().
func (a *apiServer) getGroups(ctx context.Context, subject string) ([]string, error) {
	members := a.members.ReadOnly(ctx)
	var groupsProto auth.Groups
	if err := members.Get(subject, &groupsProto); err != nil {
		if col.IsErrNotFound(err) {
			return []string{}, nil
		}
		return nil, err
	}
	return setToList(groupsProto.Groups), nil
}

// GetGroups implements the protobuf auth.GetGroups RPC
func (a *apiServer) GetGroups(ctx context.Context, req *auth.GetGroupsRequest) (resp *auth.GetGroupsResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.LogResp(req, resp, retErr, time.Since(start)) }(time.Now())
	if a.activationState() != full {
		return nil, auth.ErrNotActivated
	}

	callerInfo, err := a.getAuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}

	// must be admin or user getting your own groups (default value of
	// req.Username is the caller).
	// Note: isAdmin(subject) calls getGroups(subject), so getGroups(subject)
	// must only call isAdmin(caller) when caller != subject, or else we'll have
	// infinite recursion
	var target string
	if req.Username != "" && req.Username != callerInfo.Subject {
		isAdmin, err := a.isAdmin(ctx, callerInfo.Subject)
		if err != nil {
			return nil, err
		}
		if !isAdmin {
			return nil, &auth.ErrNotAuthorized{
				Subject: callerInfo.Subject,
				AdminOp: "GetGroups",
			}
		}
		target, err = a.canonicalizeSubject(ctx, req.Username)
		if err != nil {
			return nil, err
		}
	} else {
		target = callerInfo.Subject
	}

	groups, err := a.getGroups(ctx, target)
	if err != nil {
		return nil, err
	}
	return &auth.GetGroupsResponse{Groups: groups}, nil
}

// GetUsers implements the protobuf auth.GetUsers RPC
func (a *apiServer) GetUsers(ctx context.Context, req *auth.GetUsersRequest) (resp *auth.GetUsersResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.LogResp(req, resp, retErr, time.Since(start)) }(time.Now())
	if a.activationState() != full {
		return nil, auth.ErrNotActivated
	}

	callerInfo, err := a.getAuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	isAdmin, err := a.isAdmin(ctx, callerInfo.Subject)
	if err != nil {
		return nil, err
	}

	if !isAdmin {
		return nil, &auth.ErrNotAuthorized{
			Subject: callerInfo.Subject,
			AdminOp: "GetUsers",
		}
	}

	// Filter by group
	if req.Group != "" {
		var membersProto auth.Users
		if _, err := col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
			groups := a.groups.ReadWrite(stm)
			if err := groups.Get(req.Group, &membersProto); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return nil, err
		}

		return &auth.GetUsersResponse{Usernames: setToList(membersProto.Usernames)}, nil
	}

	membersCol := a.members.ReadOnly(ctx)
	groups := &auth.Groups{}
	var users []string
	if err := membersCol.List(groups, col.DefaultOptions, func(user string) error {
		users = append(users, user)
		return nil
	}); err != nil {
		return nil, err
	}
	return &auth.GetUsersResponse{Usernames: users}, nil
}

func setToList(set map[string]bool) []string {
	if set == nil {
		return []string{}
	}

	list := []string{}
	for elem := range set {
		list = append(list, elem)
	}
	return list
}

// hashToken converts a token to a cryptographic hash.
// We don't want to store tokens verbatim in the database, as then whoever
// that has access to the database has access to all tokens.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum)
}

// getAuthToken extracts the auth token embedded in 'ctx', if there is on
func getAuthToken(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", auth.ErrNoMetadata
	}
	if len(md[auth.ContextTokenKey]) > 1 {
		return "", fmt.Errorf("multiple authentication token keys found in context")
	} else if len(md[auth.ContextTokenKey]) == 0 {
		return "", auth.ErrNotSignedIn
	}
	return md[auth.ContextTokenKey][0], nil
}

func (a *apiServer) getAuthenticatedUser(ctx context.Context) (*auth.TokenInfo, error) {
	// TODO(msteffen) cache these lookups, especially since users always authorize
	// themselves at the beginning of a request. Don't want to look up the same
	// token -> username entry twice.
	token, err := getAuthToken(ctx)
	if err != nil {
		return nil, err
	}
	if token == a.ppsToken {
		// TODO(msteffen): This is a hack. The idea is that there is a logical user
		// entry mapping ppsToken to magicUser. Soon, magicUser will go away and
		// this check should happen in authorize
		return &auth.TokenInfo{
			Subject: magicUser,
			Source:  auth.TokenInfo_GET_TOKEN,
		}, nil
	}

	// Lookup the token
	var tokenInfo auth.TokenInfo
	if err := a.tokens.ReadOnly(ctx).Get(hashToken(token), &tokenInfo); err != nil {
		if col.IsErrNotFound(err) {
			return nil, auth.ErrBadToken
		}
		return nil, err
	}
	return &tokenInfo, nil
}

// canonicalizeSubjects applies canonicalizeSubject to a list
func (a *apiServer) canonicalizeSubjects(ctx context.Context, subjects []string) ([]string, error) {
	if subjects == nil {
		return []string{}, nil
	}

	eg := &errgroup.Group{}
	canonicalizedSubjects := make([]string, len(subjects))
	for i, subject := range subjects {
		i, subject := i, subject
		eg.Go(func() error {
			subject, err := a.canonicalizeSubject(ctx, subject)
			if err != nil {
				return err
			}
			canonicalizedSubjects[i] = subject
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return canonicalizedSubjects, nil
}

// canonicalizeSubject establishes the type of 'subject' by looking for one of
// pachyderm's subject prefixes, and then canonicalizes the subject based on
// that. If 'subject' has no prefix, they are assumed to be a GitHub user.
// TODO(msteffen): We'd like to require that subjects always have a prefix, but
// this behavior hasn't been implemented in the dash yet.
func (a *apiServer) canonicalizeSubject(ctx context.Context, subject string) (string, error) {
	colonIdx := strings.Index(subject, ":")
	if colonIdx < 0 {
		subject = auth.GitHubPrefix + subject
		colonIdx = len(auth.GitHubPrefix) - 1
	}
	prefix := subject[:colonIdx]

	// check prefix against config cache
	if config := a.getCacheConfig(); config != nil {
		for _, idp := range config.IDPs {
			if prefix == idp.Name {
				return subject, nil
			}
			if prefix == path.Join("group", idp.Name) {
				return subject, nil // TODO(msteffen): check if this IdP supports groups
			}
		}
	}

	// check against fixed prefixes
	prefix += ":" // append ":" to match constants
	switch prefix {
	case auth.GitHubPrefix:
		var err error
		subject, err = canonicalizeGitHubUsername(ctx, subject[len(auth.GitHubPrefix):])
		if err != nil {
			return "", err
		}
	case auth.PipelinePrefix, auth.RobotPrefix:
		break
	default:
		return "", fmt.Errorf("subject has unrecognized prefix: %s", subject[:colonIdx+1])
	}
	return subject, nil
}

// canonicalizeGitHubUsername corrects 'user' for case errors by looking
// up the corresponding user's GitHub profile and extracting their login ID
// from that. 'user' should not have any subject prefixes (as they are required
// to be a GitHub user).
func canonicalizeGitHubUsername(ctx context.Context, user string) (string, error) {
	if strings.Index(user, ":") >= 0 {
		return "", fmt.Errorf("invalid username has multiple prefixes: %s%s", auth.GitHubPrefix, user)
	}
	if os.Getenv(DisableAuthenticationEnvVar) == "true" {
		// authentication is off -- user might not even be real
		return auth.GitHubPrefix + user, nil
	}
	gclient := github.NewClient(http.DefaultClient)
	u, _, err := gclient.Users.Get(ctx, strings.ToLower(user))
	if err != nil {
		return "", fmt.Errorf("error canonicalizing \"%s\": %v", user, err)
	}
	return auth.GitHubPrefix + u.GetLogin(), nil
}

// GetConfiguration implements the protobuf auth.GetConfiguration RPC. Other
// users of the config in auth should get getCacheConfig and getSAMLSP rather
// than calling this handler (which will read from etcd)
func (a *apiServer) GetConfiguration(ctx context.Context, req *auth.GetConfigurationRequest) (resp *auth.GetConfigurationResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.LogResp(req, resp, retErr, time.Since(start)) }(time.Now())
	switch a.activationState() {
	case none:
		return nil, auth.ErrNotActivated
	case partial:
		return nil, auth.ErrPartiallyActivated
	}

	// Get calling user. The user must be logged in to get the cluster config
	_, err := a.getAuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}

	// Retrieve & return configuration
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	authConfigRO := a.authConfig.ReadOnly(ctx)

	var currentCfg auth.AuthConfig
	if err := authConfigRO.Get(configKey, &currentCfg); err != nil && !col.IsErrNotFound(err) {
		return nil, err
	} else if col.IsErrNotFound(err) {
		currentCfg = defaultAuthConfig
	} else {
		cacheCfg := a.getCacheConfig()
		if cacheCfg == nil || cacheCfg.Version < currentCfg.LiveConfigVersion {
			var cacheVersion int64
			if cacheCfg != nil {
				cacheVersion = cacheCfg.Version
			}
			logrus.Printf("current config (v.%d) is newer than cache (v.%d); attempting to update cache",
				currentCfg.LiveConfigVersion, cacheVersion)
			// possible race--config could be updated between getCacheConfig() and
			// here, but setCacheConfig validates the write.
			if err := a.setCacheConfig(&currentCfg); err != nil {
				logrus.Warnf("could not update SAML service with new config: %v", err)
			}
		} else if cacheCfg.Version > currentCfg.LiveConfigVersion {
			logrus.Warnln("config cache is NEWER than live config; this shouldn't happen")
		}
	}
	return &auth.GetConfigurationResponse{
		Configuration: &currentCfg,
	}, nil
}

// SetConfiguration implements the protobuf auth.SetConfiguration RPC
func (a *apiServer) SetConfiguration(ctx context.Context, req *auth.SetConfigurationRequest) (resp *auth.SetConfigurationResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.LogResp(req, resp, retErr, time.Since(start)) }(time.Now())
	switch a.activationState() {
	case none:
		return nil, auth.ErrNotActivated
	case partial:
		return nil, auth.ErrPartiallyActivated
	}

	// Get calling user. The user must be an admin to set the cluster config
	callerInfo, err := a.getAuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	isAdmin, err := a.isAdmin(ctx, callerInfo.Subject)
	if err != nil {
		return nil, err
	}
	if !isAdmin {
		return nil, &auth.ErrNotAuthorized{
			Subject: callerInfo.Subject,
			AdminOp: "SetConfiguration",
		}
	}

	var reqConfigVersion int64
	var configToStore *auth.AuthConfig
	if req.Configuration != nil {
		reqConfigVersion = req.Configuration.LiveConfigVersion
		// Validate new config
		canonicalConfig, err := validateConfig(req.Configuration, external)
		if err != nil {
			return nil, err
		}
		configToStore, err = canonicalConfig.ToProto()
		if err != nil {
			return nil, err
		}
	} else {
		// Explicitly store default auth config so that config version is retained &
		// continues increasing monotonically. Don't set reqConfigVersion, though--
		// setting an empty config is a blind write (req.Configuration.Version == 0)
		configToStore = proto.Clone(&defaultAuthConfig).(*auth.AuthConfig)
	}

	// upsert new config
	if _, err := col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
		var liveConfig auth.AuthConfig
		return a.authConfig.ReadWrite(stm).Upsert(configKey, &liveConfig, func() error {
			if liveConfig.LiveConfigVersion == 0 {
				liveConfig = defaultAuthConfig // no config in etcd--assume default cfg
			}
			liveConfigVersion := liveConfig.LiveConfigVersion
			if reqConfigVersion > 0 && liveConfigVersion != reqConfigVersion {
				return fmt.Errorf("expected config version %d, but live config has version %d",
					req.Configuration.LiveConfigVersion, liveConfigVersion)
			}
			liveConfig.Reset()
			liveConfig = *configToStore
			liveConfig.LiveConfigVersion = liveConfigVersion + 1
			return nil
		})
	}); err != nil {
		return nil, err
	}
	return &auth.SetConfigurationResponse{}, nil
}
