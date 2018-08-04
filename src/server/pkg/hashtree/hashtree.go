package hashtree

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	pathlib "path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pachyderm/pachyderm/src/client/pfs"

	globlib "github.com/gobwas/glob"
	"github.com/golang/protobuf/proto"
)

type nodetype uint8

const (
	none         nodetype = iota // No file is present at this point in the tree
	directory                    // The file at this point in the tree is a directory
	file                         // ... is a regular file
	unrecognized                 // ... is an an unknown type
)

func (n *NodeProto) nodetype() nodetype {
	switch {
	case n == nil || (n.DirNode == nil && n.FileNode == nil):
		return none
	case n.DirNode != nil:
		return directory
	case n.FileNode != nil:
		return file
	default:
		return unrecognized
	}
}

func (n *OpenNode) nodetype() nodetype {
	switch {
	case n == nil:
		return none
	case n.DirNode != nil:
		return directory
	case n.FileNode != nil:
		return file
	default:
		return unrecognized
	}
}

func (n nodetype) tostring() string {
	switch n {
	case none:
		return "none"
	case directory:
		return "directory"
	case file:
		return "file"
	default:
		return "unknown"
	}
}

// Serialize serializes a HashTree so that it can be persisted. Also see
// Deserialize(bytes).
func Serialize(h HashTree) ([]byte, error) {
	tree, ok := h.(*HashTreeProto)
	if !ok {
		return nil, fmt.Errorf("HashTree is of the wrong concrete type")
	}
	return tree.Marshal()
}

// Deserialize deserializes a hash tree so that it can be read or modified.
func Deserialize(serialized []byte) (HashTree, error) {
	h := &HashTreeProto{}
	if err := h.Unmarshal(serialized); err != nil {
		return nil, err
	}
	if h.Version != 1 {
		return nil, errorf(Unsupported, "unsupported HashTreeProto "+
			"version %d", h.Version)
	}
	return h, nil
}

// Open makes a deep copy of the HashTree and returns the copy
func (h *HashTreeProto) Open() OpenHashTree {
	// create a deep copy of 'h' with proto.Clone
	h2 := proto.Clone(h).(*HashTreeProto)
	// make a shallow copy of 'innerh' (effectively) and return that
	h3 := &hashtree{
		fs:      h2.Fs,
		changed: make(map[string]bool),
	}
	if h3.fs == nil {
		h3.fs = make(map[string]*NodeProto)
	}
	return h3
}

func get(fs map[string]*NodeProto, path string) (*NodeProto, error) {
	path = clean(path)

	node, ok := fs[path]
	for k, v := range fs {
		fmt.Printf("path (%v) value (%v)\n", k, v)
	}
	if !ok {
		fmt.Printf("couldnt find path %v in fs %v\n", path, fs)
		return nil, errorf(PathNotFound, "file \"%s\" not found", path)
	}
	return node, nil
}

// Get retrieves the contents of a file.
func (h *HashTreeProto) Get(path string) (*NodeProto, error) {
	return get(h.Fs, path)
}

func list(fs map[string]*NodeProto, path string) ([]*NodeProto, error) {
	path = clean(path)

	node, err := get(fs, path)
	if err != nil {
		return nil, err
	}
	d := node.DirNode
	if d == nil {
		return nil, errorf(PathConflict, "the file at \"%s\" is not a directory",
			path)
	}
	var ok bool
	result := make([]*NodeProto, len(d.Children))
	for i, child := range d.Children {
		result[i], ok = fs[join(path, child)]
		if !ok {
			return nil, errorf(Internal, "could not find file for the child \"%s\" "+
				"while listing \"%s\"", join(path, child), path)
		}
	}
	return result, nil
}

// List retrieves the list of files and subdirectories of the directory at
// 'path'.
func (h *HashTreeProto) List(path string) ([]*NodeProto, error) {
	return list(h.Fs, path)
}

func glob(fs map[string]*NodeProto, pattern string) (map[string]*NodeProto, error) {
	fmt.Printf("in glob()\n")
	if !isGlob(pattern) {
		node, err := get(fs, pattern)
		if err != nil {
			return nil, err
		}
		return map[string]*NodeProto{clean(pattern): node}, nil
	}

	// "*" should be an allowed pattern, but our paths always start with "/", so
	// modify the pattern to fit our path structure.
	pattern = clean(pattern)

	g, err := globlib.Compile(pattern, '/')
	if err != nil {
		return nil, errorf(MalformedGlob, err.Error())
	}

	res := make(map[string]*NodeProto)
	for path, node := range fs {
		fmt.Printf("glob walking over path: %v\n", path)
		if g.Match(path) {
			fmt.Printf("path %v matched pattern %v\n", path, pattern)
			res[path] = node
		}
	}

	return res, nil
}

var globRegex = regexp.MustCompile(`[*?\[\]\{\}!]`)

// isGlob checks if the pattern contains a glob character
func isGlob(pattern string) bool {
	return globRegex.Match([]byte(pattern))
}

// Glob returns a map of file/directory paths to nodes that match 'pattern'.
func (h *HashTreeProto) Glob(pattern string) (map[string]*NodeProto, error) {
	return glob(h.Fs, pattern)
}

func size(fs map[string]*NodeProto) int64 {
	rootNode, ok := fs[clean("/")]
	if !ok {
		return 0
	}
	return rootNode.SubtreeSize
}

// FSSize returns the size of the file system that the hashtree represents.
func (h *HashTreeProto) FSSize() int64 {
	return size(h.Fs)
}

func walk(fs map[string]*NodeProto, path string, f func(string, *NodeProto) error) error {
	path = clean(path)
	if node, ok := fs[path]; ok && node.FileNode != nil {
		return f(path, node)
	} else if !ok {
		return errorf(PathNotFound, "file \"%s\" not found", path)
	}
	for rangePath, node := range fs {
		if rangePath == "" {
			rangePath = "/"
		}
		if !strings.HasPrefix(rangePath, path) {
			continue
		}
		if err := f(rangePath, node); err != nil {
			return err
		}
	}
	return nil
}

// Walk implements HashTree.Walk
func (h *HashTreeProto) Walk(path string, f func(string, *NodeProto) error) error {
	return walk(h.Fs, path, f)
}

func diff(new HashTree, old HashTree, newPath string, oldPath string, recursiveDepth int64, f func(string, *NodeProto, bool) error) error {
	newNode, err := new.Get(newPath)
	if err != nil && Code(err) != PathNotFound {
		return err
	}
	oldNode, err := old.Get(oldPath)
	if err != nil && Code(err) != PathNotFound {
		return err
	}
	if (newNode == nil && oldNode == nil) ||
		(newNode != nil && oldNode != nil && bytes.Equal(newNode.Hash, oldNode.Hash)) {
		return nil
	}
	children := make(map[string]bool)
	if newNode != nil {
		if newNode.FileNode != nil || recursiveDepth == 0 {
			if err := f(newPath, newNode, true); err != nil {
				return err
			}
		} else if newNode.DirNode != nil {
			for _, child := range newNode.DirNode.Children {
				children[child] = true
			}
		}
	}
	if oldNode != nil {
		if oldNode.FileNode != nil || recursiveDepth == 0 {
			if err := f(oldPath, oldNode, false); err != nil {
				return err
			}
		} else if oldNode.DirNode != nil {
			for _, child := range oldNode.DirNode.Children {
				children[child] = true
			}
		}
	}
	if recursiveDepth > 0 || recursiveDepth == -1 {
		newDepth := recursiveDepth
		if recursiveDepth > 0 {
			newDepth--
		}
		for child := range children {
			if err := diff(new, old, pathlib.Join(newPath, child), pathlib.Join(oldPath, child), newDepth, f); err != nil {
				return err
			}
		}
	}
	return nil
}

// Diff implements HashTree.Diff
func (h *HashTreeProto) Diff(old HashTree, newPath string, oldPath string, recursiveDepth int64, f func(string, *NodeProto, bool) error) error {
	return diff(h, old, newPath, oldPath, recursiveDepth, f)
}

// hashtree is an implementation of the HashTree and OpenHashTree interfaces.
// It's intended to describe the state of a single commit C, in a repo R.
type hashtree struct {
	// fs (short for files) maps the path of each file F in the repo R to a
	// protobuf message describing F. It's equivalent to HashTree.Fs.
	fs map[string]*NodeProto

	// changed maps a path P to 'true' if P or one of its children has been
	// modified in 'fs', and its hash needs to be updated.
	changed map[string]bool
}

// Open returns the hashtree since it's already an OpenHashTree
func (h *hashtree) Open() OpenHashTree {
	return h
}

// Get retrieves the contents of a file.
func (h *hashtree) Get(path string) (*NodeProto, error) {
	return get(h.fs, path)
}

// List retrieves the list of files and subdirectories of the directory at
// 'path'.
func (h *hashtree) List(path string) ([]*NodeProto, error) {
	return list(h.fs, path)
}

// Glob returns a map of file/directory paths to nodes that match 'pattern'.
func (h *hashtree) Glob(pattern string) (map[string]*NodeProto, error) {
	return glob(h.fs, pattern)
}

// FSSize returns the size of the file system that the hashtree represents.
func (h *hashtree) FSSize() int64 {
	return size(h.fs)
}

// Walk implements HashTree.Walk
func (h *hashtree) Walk(path string, f func(string, *NodeProto) error) error {
	return walk(h.fs, path, f)
}

// Diff implements HashTree.Diff
func (h *hashtree) Diff(old HashTree, newPath string, oldPath string, recursiveDepth int64, f func(string, *NodeProto, bool) error) error {
	return diff(h, old, newPath, oldPath, recursiveDepth, f)
}

// clone makes a deep copy of 'h' and returns it. This performs one fewer copy
// than h.Finish().Open()
func (h *hashtree) clone() (*hashtree, error) {
	h2, err := h.Finish()
	if err != nil {
		return nil, errorf(Internal,
			"could not Finish() hashtree in clone(): %s", err)
	}
	h3, ok := h2.(*HashTreeProto)
	if !ok {
		return nil, errorf(Internal,
			"could not convert HashTree to *HashTreeProto in clone()")
	}
	result := &hashtree{
		fs:      h3.Fs,
		changed: make(map[string]bool),
	}
	if result.fs == nil {
		result.fs = make(map[string]*NodeProto)
	}
	return result, nil
}

// NewHashTree creates a new hash tree implementing Interface.
func NewHashTree() OpenHashTree {
	result := &hashtree{
		fs:      make(map[string]*NodeProto),
		changed: make(map[string]bool),
	}
	result.PutDir("/")
	return result
}

// canonicalize updates the hash of the node N at 'path'. If N is a directory
// canonicalize will also update the hash of all of N's children recursively.
// Thus, h.canonicalize("/") will update the Hash field of all nodes in h,
// making the whole tree consistent.
func (h *hashtree) canonicalize(path string) error {
	path = clean(path)
	if !h.changed[path] {
		return nil // Node is already canonical
	}
	n, ok := h.fs[path]
	if !ok {
		return errorf(Internal, "file \"%s\" not found; cannot canonicalize", path)
	}

	// Compute hash of 'n'
	hash := sha256.New()
	switch n.nodetype() {
	case directory:
		// Compute n.Hash by concatenating name + hash of all children of n.DirNode
		// Note that PutFile keeps n.DirNode.Children sorted, so the order is
		// stable.
		for _, child := range n.DirNode.Children {
			childpath := join(path, child)
			if err := h.canonicalize(childpath); err != nil {
				return err
			}
			childnode, ok := h.fs[childpath]
			if !ok {
				return errorf(Internal, "could not find file for \"%s\" while "+
					"updating hash of \"%s\"", join(path, child), path)
			}
			// append child.Name and child.Hash to b
			hash.Write([]byte(fmt.Sprintf("%s:%s:", childnode.Name, childnode.Hash)))
		}
	case file:
		// Compute n.Hash by concatenating all BlockRef hashes in n.FileNode.
		for _, object := range n.FileNode.Objects {
			hash.Write([]byte(object.Hash))
		}
	default:
		return errorf(Internal,
			"malformed file at \"%s\" is neither a file nor a directory", path)
	}

	// Update hash of 'n'
	n.Hash = hash.Sum(nil)
	delete(h.changed, path)
	return nil
}

// updateFn is used by 'visit'. The first parameter is the node being visited,
// the second parameter is the path of that node, and the third parameter is the
// child of that node from the 'path' argument to 'visit'.
//
// The *NodeProto argument is guaranteed to have DirNode set (if it's not nil)--visit
// returns a 'PathConflict' error otherwise.
type updateFn func(*NodeProto, string, string) error

// This can be passed to visit() to detect PathConflict errors early
func nop(*NodeProto, string, string) error {
	return nil
}

// visit visits every ancestor of 'path' (excluding 'path' itself), leaf to
// root (i.e.  end of 'path' to beginning), and calls 'update' on each node
// along the way. For example, if 'visit' is called with 'path'="/path/to/file",
// then updateFn is called as follows:
//
// 1. update(node at "/path/to" or nil, "/path/to", "file")
// 2. update(node at "/path"    or nil, "/path",    "to")
// 3. update(node at "/"        or nil, "",         "path")
//
// This is useful for propagating changes to size upwards.
func (h *hashtree) visit(path string, update updateFn) error {
	for path != "" {
		parent, child := split(path)
		pnode, ok := h.fs[parent]
		if ok && pnode.nodetype() != directory {
			return errorf(PathConflict, "attempted to visit \"%s\", but it's not a "+
				"directory", path)
		}
		if err := update(pnode, parent, child); err != nil {
			return err
		}
		path = parent
	}
	return nil
}

// removeFromMap removes the node at 'path' from h.fs if it's present, along
// with all of its children, recursively.
//
// This will not update the hash of any parent of 'path'. This helps us avoid
// updating the hash of path's parents unnecessarily; if 'path' is a directory
// with e.g. 10k children, updating the parents' hashes after all files have
// been removed from h.fs (instead of updating all parents' hashesafter
// removing each file) may save substantial time.
func (h *hashtree) removeFromMap(path string) error {
	n, ok := h.fs[path]
	if !ok {
		return nil
	}

	switch n.nodetype() {
	case file:
		delete(h.fs, path)
	case directory:
		for _, child := range n.DirNode.Children {
			if err := h.removeFromMap(join(path, child)); err != nil {
				return err
			}
		}
		delete(h.fs, path)
	case unrecognized:
		return errorf(Internal,
			"malformed file at \"%s\": it's neither a regular-file nor a directory", path)
	}
	return nil
}

// Finish makes a deep copy of the OpenHashTree, updates all of the hashes in
// the copy, and returns the copy
func (h *hashtree) Finish() (HashTree, error) {
	if err := h.canonicalize(""); err != nil {
		return nil, err
	}
	// Create a shallow copy of 'h'
	innerp := &HashTreeProto{
		Fs:      h.fs,
		Version: 1,
	}
	// convert the shallow copy of 'h' to a deep copy with proto.Clone()
	return proto.Clone(innerp).(*HashTreeProto), nil
}

// PutFile appends data to a file (and creates the file if it doesn't exist).
func (h *hashtree) PutFile(path string, objects []*pfs.Object, size int64) error {
	return h.putFile(path, objects, nil, size, nil, nil, 0)
}

func (h *hashtree) PutFileOverwrite(path string, objects []*pfs.Object, overwriteIndex *pfs.OverwriteIndex, sizeDelta int64) error {
	return h.putFile(path, objects, overwriteIndex, sizeDelta, nil, nil, 0)
}

func (h *hashtree) PutFileSplit(path string, objects []*pfs.Object, size int64, header *pfs.Object, footer *pfs.Object, headerFooterSize int64) error {
	return h.putFile(path, objects, nil, size, header, footer, headerFooterSize)
}

// PutFile appends data to a file (and creates the file if it doesn't exist).
func (h *hashtree) putFile(path string, objects []*pfs.Object, overwriteIndex *pfs.OverwriteIndex, sizeDelta int64, header *pfs.Object, footer *pfs.Object, headerFooterSize int64) error {
	path = clean(path)

	fmt.Printf("in tree putFile() modifying path %v\n", path)
	// Detect any path conflicts before modifying 'h'
	if err := h.visit(path, nop); err != nil {
		return err
	}

	if len(objects) > 0 {
		fmt.Printf("creating node\n")
		// Get/Create file node to which we'll append 'objects'
		node, ok := h.fs[path]
		if !ok {
			node = &NodeProto{
				Name:     base(path),
				FileNode: &FileNodeProto{},
			}
			h.fs[path] = node
		} else if node.nodetype() != file {
			return errorf(PathConflict, "could not put file at \"%s\"; a file of "+
				"type %s is already there", path, node.nodetype().tostring())
		}
		fmt.Printf("created node %v\n", node)

		// Append new objects.  Remove existing objects if overwriting.
		if overwriteIndex != nil && overwriteIndex.Index <= int64(len(node.FileNode.Objects)) {
			node.FileNode.Objects = node.FileNode.Objects[:overwriteIndex.Index]
		}
		node.SubtreeSize += sizeDelta + headerFooterSize
		node.FileNode.Objects = append(node.FileNode.Objects, objects...)
		h.changed[path] = true
		fmt.Printf("in tree putFile() .... going to set parent\n")
		// Add 'path' to parent (if it's new) & mark nodes as 'changed' back to root
		err := h.visit(path, func(node *NodeProto, parent, child string) error {
			if node == nil {
				node = &NodeProto{
					Name:    base(parent),
					DirNode: &DirectoryNodeProto{},
				}
				if parent == filepath.Dir(path) {
					node.SubtreeSize = headerFooterSize
					node.DirNode.Header = header
					node.DirNode.Footer = footer
				}
				fmt.Printf("set parent (%v) to %v\n", parent, node)
				h.fs[parent] = node
			}
			insertStr(&node.DirNode.Children, child)
			node.SubtreeSize += sizeDelta
			h.changed[parent] = true
			return nil
		})
		fmt.Printf("error visiting nodes: %v\n", err)
		return err
	} else {
		fmt.Printf("hashtree putfile writing directly to existing dir node\n")
		// Get/Create dir node to which we'll add header/footer
		node, ok := h.fs[path]
		fmt.Printf("node (%v), ok (%v)\n", node, ok)
		if !ok {
			node = &NodeProto{
				Name:    base(path),
				DirNode: &DirectoryNodeProto{},
			}
			h.fs[path] = node
		} else if node.nodetype() != directory {
			return errorf(PathConflict, "could not put dir at \"%s\"; a file of "+
				"type %s is already there", path, node.nodetype().tostring())
		}
		node.DirNode.Header = header
		node.DirNode.Footer = footer
		node.SubtreeSize += headerFooterSize
		fmt.Printf("created node %v\n", node)
		h.changed[path] = true
		fmt.Printf("in tree putFile() .... going to set parent\n")
		// Add 'path' to parent (if it's new) & mark nodes as 'changed' back to root
		err := h.visit(path, func(node *NodeProto, parent, child string) error {
			if node == nil {
				node = &NodeProto{
					Name:    base(parent),
					DirNode: &DirectoryNodeProto{},
				}
				fmt.Printf("set dir parent (%v) to %v\n", parent, node)
				h.fs[parent] = node
			}
			insertStr(&node.DirNode.Children, child)
			node.SubtreeSize += headerFooterSize
			h.changed[parent] = true
			return nil
		})
		return err
	}

	return nil
}

// PutDir creates a directory (or does nothing if one exists).
func (h *hashtree) PutDir(path string) error {
	path = clean(path)

	// Detect any path conflicts before modifying 'h'
	if err := h.visit(path, nop); err != nil {
		return err
	}

	// Create orphaned directory at 'path' (or end early if a directory is there)
	if node, ok := h.fs[path]; ok {
		if node.nodetype() == directory {
			return nil
		} else if node.nodetype() != none {
			return errorf(PathConflict, "could not create directory at \"%s\"; a "+
				"file of type %s is already there", path, node.nodetype().tostring())
		}
	}
	h.fs[path] = &NodeProto{
		Name:    base(path),
		DirNode: &DirectoryNodeProto{},
	}
	h.changed[path] = true

	// Add 'path' to parent & update hashes back to root
	return h.visit(path, func(node *NodeProto, parent, child string) error {
		if node == nil {
			node = &NodeProto{
				Name:    base(parent),
				DirNode: &DirectoryNodeProto{},
			}
			h.fs[parent] = node
		}
		insertStr(&node.DirNode.Children, child)
		h.changed[parent] = true
		return nil
	})
}

// DeleteFile deletes a regular file or directory (along with its children).
func (h *hashtree) DeleteFile(path string) error {
	path = clean(path)

	// Delete root means delete all files
	if path == "" {
		path = "*"
	}
	paths, err := h.Glob(path)

	// Deleting a non-existent file should be a no-op
	if err != nil && Code(err) != PathNotFound {
		return err
	}

	for path := range paths {
		// Check if the file has been deleted already
		if _, ok := h.fs[path]; !ok {
			continue
		}

		// Remove 'path' and all nodes underneath it from h.fs
		h.removeFromMap(path) // Deletes children recursively
		size := paths[path].SubtreeSize

		// Remove 'path' from its parent directory
		parent, child := split(path)
		node, ok := h.fs[parent]
		if !ok {
			return errorf(Internal, "delete discovered orphaned file \"%s\"", path)
		}
		if node.DirNode == nil {
			return errorf(Internal, "file at \"%s\" is a regular-file, but \"%s\" already exists "+
				"under it (likely an uncaught PathConflict in prior PutFile or Merge)", path, node.DirNode)
		}
		if !removeStr(&node.DirNode.Children, child) {
			return errorf(Internal, "parent of \"%s\" does not contain it", path)
		}
		// Mark nodes as 'changed' back to root
		if err := h.visit(path, func(node *NodeProto, parent, child string) error {
			if node == nil {
				return errorf(Internal,
					"encountered orphaned file \"%s\" while deleting \"%s\"", path,
					join(parent, child))
			}
			node.SubtreeSize -= size
			h.changed[parent] = true
			return nil
		}); err != nil {
			return err
		}
	}

	return nil
}

// GetOpen retrieves a file.
func (h *hashtree) GetOpen(path string) (*OpenNode, error) {
	path = clean(path)
	np, ok := h.fs[path]
	if !ok {
		return nil, errorf(PathNotFound, "file \"%s\" not found", path)
	}
	return &OpenNode{
		Name:     np.Name,
		Size:     np.SubtreeSize,
		FileNode: np.FileNode,
		DirNode:  np.DirNode,
	}, nil
}

// mergeNode merges the node at 'path' from the trees in 'srcs' into 'h'.
func (h *hashtree) mergeNode(path string, srcs []HashTree) (int64, error) {
	path = clean(path)
	// Get the node at path in 'h' and determine its type (i.e. file, dir)
	destNode, ok := h.fs[path]
	if !ok {
		destNode = &NodeProto{
			Name:        base(path),
			SubtreeSize: 0,
			Hash:        nil,
			FileNode:    nil,
			DirNode:     nil,
		}
		h.fs[path] = destNode
	} else if destNode.nodetype() == unrecognized {
		return 0, errorf(Internal, "malformed file at \"%s\" in destination "+
			"hashtree is neither a regular-file nor a directory", path)
	}
	sizeDelta := int64(0) // We return this to propagate file additions upwards

	// Get node at 'path' in all 'srcs'. All such nodes must have the same type as
	// each other and the same type as 'destNode'
	pathtype := destNode.nodetype() // All nodes in 'srcs' must have same type
	// childrenToTrees is a reverse index from [child of 'path'] to [trees that
	// contain it].
	// - childrenToTrees will only be used if 'path' is a directory in all
	//   'srcs' (but it's convenient to build it here, before we've checked them
	//   all)
	// - We need to group trees by common children, so that children present in
	//   multiple 'srcNodes' are only merged once
	// - if every srcNode has a unique file /foo/shard-xxxxx (00000 to 99999)
	//   then we'd call mergeNode("/foo") 100k times, once for each tree in
	//   'srcs', while running mergeNode("/").
	// - We also can't pass all of 'srcs' to mergeNode("/foo"), as otherwise
	//   mergeNode("/foo/shard-xxxxx") will have to filter through all 100k trees
	//   for each shard-xxxxx (only one of which contains the file being merged),
	//   and we'd have an O(n^2) algorithm; too slow when merging 100k trees)
	childrenToTrees := make(map[string][]HashTree)
	// Amount of data being added to node at 'path' in 'h'
	for _, src := range srcs {
		// Get the node at 'path' from 'src', and initialize 'h' at 'path' if needed
		n, err := src.Get(path)
		if Code(err) == PathNotFound {
			continue
		}
		if err != nil && Code(err) != PathNotFound {
			return 0, err
		}
		if pathtype == none {
			// 'h' is uninitialized at this path
			if n.nodetype() == directory {
				destNode.DirNode = &DirectoryNodeProto{}
			} else if n.nodetype() == file {
				destNode.FileNode = &FileNodeProto{}
			} else {
				return 0, errorf(Internal, "could not merge unrecognized file type at "+
					"\"%s\", which is neither a file nore a directory", path)
			}
			pathtype = n.nodetype()
		} else if pathtype != n.nodetype() {
			return sizeDelta, errorf(PathConflict, "could not merge path \"%s\" "+
				"which is a regular-file in some hashtrees and a directory in others", path)
		}
		switch n.nodetype() {
		case directory:
			// Instead of merging here, we build a reverse-index and merge below
			for _, c := range n.DirNode.Children {
				childrenToTrees[c] = append(childrenToTrees[c], src)
			}
		case file:
			// Append new objects, and update size of target node (since that can't be
			// done in canonicalize)
			destNode.FileNode.Objects = append(destNode.FileNode.Objects,
				n.FileNode.Objects...)
			sizeDelta += n.SubtreeSize
		default:
			return sizeDelta, errorf(Internal, "malformed file at \"%s\" in source "+
				"hashtree is neither a regular-file nor a directory", path)
		}
	}

	// If this is a directory, go back and merge all children encountered above
	if pathtype == directory {
		// Merge all children (collected in childrenToTrees)
		for c, cSrcs := range childrenToTrees {
			childSizeDelta, err := h.mergeNode(join(path, c), cSrcs)
			if err != nil {
				return sizeDelta, err
			}
			sizeDelta += childSizeDelta
			insertStr(&destNode.DirNode.Children, c)
		}
	}
	// Update the size of destNode, and mark it changed
	destNode.SubtreeSize += sizeDelta
	h.changed[path] = true
	return sizeDelta, nil
}

// Merge merges the HashTrees in 'trees' into 'h'. The result is nil if no
// errors are encountered while merging any tree, or else a new error e, where:
// - Code(e) is the error code of the first error encountered
// - e.Error() contains the error messages of the first 10 errors encountered
func (h *hashtree) Merge(trees ...HashTree) error {
	// Skip empty trees
	var nonEmptyTrees []HashTree
	for _, tree := range trees {
		_, err := tree.Get("/")
		if err != nil {
			continue
		}
		nonEmptyTrees = append(nonEmptyTrees, tree)
	}

	if len(nonEmptyTrees) == 0 {
		return nil
	}
	if _, err := h.mergeNode("/", nonEmptyTrees); err != nil {
		return err
	}
	return nil
}