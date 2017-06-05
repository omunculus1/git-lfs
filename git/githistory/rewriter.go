package githistory

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/git-lfs/git-lfs/filepathfilter"
	"github.com/git-lfs/git-lfs/git"
	"github.com/git-lfs/git-lfs/git/odb"
)

// Rewriter allows rewriting topologically equivalent Git histories
// between two revisions.
type Rewriter struct {
	// mu guards entries and commits (see below)
	mu *sync.Mutex
	// entries is a mapping of old tree entries to new (rewritten) ones.
	// Since TreeEntry contains a []byte (and is therefore not a key-able
	// type), a unique TreeEntry -> string function is used for map keys.
	entries map[string]*odb.TreeEntry
	// commits is a mapping of old commit SHAs to new ones, where the ASCII
	// hex encoding of the SHA1 values are used as map keys.
	commits map[string][]byte
	// filter is an optional value used to specify which tree entries
	// (blobs, subtrees) are modifiable given a BlobFn. If non-nil, this
	// filter will cull out any unmodifiable subtrees and blobs.
	filter *filepathfilter.Filter
	// db is the *ObjectDatabase from which blobs, commits, and trees are
	// loaded from.
	db *odb.ObjectDatabase
}

// RewriteOptions is an options type given to the Rewrite() function.
type RewriteOptions struct {
	// Left is the starting commit.
	Left string
	// Right is the ending commit.
	Right string

	// BlobFn specifies a function to rewrite blobs.
	//
	// It is called once per unique, unchanged path. That is to say, if
	// a/foo and a/bar contain identical contents, the BlobFn will be called
	// twice: once for a/foo and once for a/bar, but no more on each blob
	// for subsequent revisions, so long as each entry remains unchanged.
	BlobFn BlobRewriteFn
}

// BlobRewriteFn is a mapping function that takes a given blob and returns a
// new, modified blob. If it returns an error, the new blob will not be written
// and instead the error will be returned from the Rewrite() function.
//
// Invocations of an instance of BlobRewriteFn are not expected to store the
// returned blobs in the *git/odb.ObjectDatabase.
//
// The path argument is given to be an absolute path to the tree entry being
// rewritten, where the repository root is the root of the path given. For
// instance, a file "b.txt" in directory "dir" would be given as "dir/b.txt",
// where as a file "a.txt" in the root would be given as "a.txt".
//
// As above, the path separators are OS specific, and equivalent to the result
// of filepath.Join(...) or os.PathSeparator.
type BlobRewriteFn func(path string, b *odb.Blob) (*odb.Blob, error)

type rewriterOption func(*Rewriter)

var (
	// WithFilter is an optional argument given to the NewRewriter
	// constructor function to limit invocations of the BlobRewriteFn to
	// only pathspecs that match the given *filepathfilter.Filter.
	WithFilter = func(filter *filepathfilter.Filter) rewriterOption {
		return func(r *Rewriter) {
			r.filter = filter
		}
	}
)

// NewRewriter constructs a *Rewriter from the given *ObjectDatabase instance.
func NewRewriter(db *odb.ObjectDatabase, opts ...rewriterOption) *Rewriter {
	rewriter := &Rewriter{
		mu:      new(sync.Mutex),
		entries: make(map[string]*odb.TreeEntry),
		commits: make(map[string][]byte),

		db: db,
	}

	for _, opt := range opts {
		opt(rewriter)
	}
	return rewriter
}

// Rewrite rewrites the range of commits given by *RewriteOptions.{Left,Right}
// using the BlobRewriteFn to rewrite the individual blobs.
func (r *Rewriter) Rewrite(opt *RewriteOptions) ([]byte, error) {
	// First, construct a scanner to iterate through the range of commits to
	// rewrite.
	scanner, err := git.NewRevListScanner(opt.Left, opt.Right, r.scannerOpts())
	if err != nil {
		return nil, err
	}

	// Keep track of the last commit that we rewrote. Callers often want
	// this so that they can perform a git-update-ref(1).
	var tip []byte
	for scanner.Scan() {
		// Load the original commit to access the data necessary in
		// order to rewrite it.
		original, err := r.db.Commit(scanner.OID())
		if err != nil {
			return nil, err
		}

		// Rewrite the tree given at that commit.
		rewrittenTree, err := r.rewriteTree(original.TreeID, "", opt.BlobFn)
		if err != nil {
			return nil, err
		}

		// Create a new list of parents from the original commit to
		// point at the rewritten parents in order to create a
		// topologically equivalent DAG.
		//
		// This operation is safe since we are visiting the commits in
		// reverse topological order and therefore have seen all parents
		// before children (in other words, r.uncacheCommit(parent) will
		// always return a value).
		rewrittenParents := make([][]byte, 0, len(original.ParentIDs))
		for _, parent := range original.ParentIDs {
			rewrittenParents = append(rewrittenParents, r.uncacheCommit(parent))
		}

		// Construct a new commit using the original header information,
		// but the rewritten set of parents as well as root tree.
		rewrittenCommit, err := r.db.WriteCommit(&odb.Commit{
			Author:       original.Author,
			Committer:    original.Committer,
			ExtraHeaders: original.ExtraHeaders,
			Message:      original.Message,

			ParentIDs: rewrittenParents,
			TreeID:    rewrittenTree,
		})
		if err != nil {
			return nil, err
		}

		// Cache that commit so that we can reassign children of this
		// commit.
		r.cacheCommit(scanner.OID(), rewrittenCommit)

		// Move the tip forward.
		tip = rewrittenCommit
	}

	if err = scanner.Err(); err != nil {
		return nil, err
	}
	return tip, err
}

// rewriteTree is a recursive function which rewrites a tree given by the ID
// "sha" and path "path". It uses the given BlobRewriteFn to rewrite all blobs
// within the tree, either calling that function or recurring down into subtrees
// by re-assigning the SHA.
//
// It returns the new SHA of the rewritten tree, or an error if the tree was
// unable to be rewritten.
func (r *Rewriter) rewriteTree(sha []byte, path string, fn BlobRewriteFn) ([]byte, error) {
	tree, err := r.db.Tree(sha)
	if err != nil {
		return nil, err
	}

	entries := make([]*odb.TreeEntry, 0, len(tree.Entries))
	for _, entry := range tree.Entries {
		path := filepath.Join(path, entry.Name)

		if !r.filter.Allows(path) {
			entries = append(entries, entry)
			continue
		}

		if cached := r.uncacheEntry(entry); cached != nil {
			entries = append(entries, cached)
			continue
		}

		var oid []byte

		switch entry.Type {
		case odb.BlobObjectType:
			oid, err = r.rewriteBlob(entry.Oid, path, fn)
		case odb.TreeObjectType:
			oid, err = r.rewriteTree(entry.Oid, path, fn)
		default:
			oid = entry.Oid

		}
		if err != nil {
			return nil, err
		}

		entries = append(entries, r.cacheEntry(entry, &odb.TreeEntry{
			Filemode: entry.Filemode,
			Name:     entry.Name,
			Type:     entry.Type,
			Oid:      oid,
		}))
	}

	return r.db.WriteTree(&odb.Tree{Entries: entries})
}

// rewriteBlob calls the given BlobRewriteFn "fn" on a blob given in the object
// database by the SHA1 "from" []byte. It writes and returns the new blob SHA,
// or an error if either the BlobRewriteFn returned one, or if the object could
// not be loaded/saved.
func (r *Rewriter) rewriteBlob(from []byte, path string, fn BlobRewriteFn) ([]byte, error) {
	blob, err := r.db.Blob(from)
	if err != nil {
		return nil, err
	}

	b, err := fn(path, blob)
	if err != nil {
		return nil, err
	}
	return r.db.WriteBlob(b)
}

// scannerOpts returns a *git.ScanRefsOptions instance to be given to the
// *git.RevListScanner.
//
// If the database this *Rewriter is operating in a given root (not in memory)
// it re-assigns the working directory to be there.
func (r *Rewriter) scannerOpts() *git.ScanRefsOptions {
	opts := &git.ScanRefsOptions{
		Mode:        git.ScanRefsMode,
		Order:       git.TopoRevListOrder,
		Reverse:     true,
		CommitsOnly: true,

		SkippedRefs: make([]string, 0),
		Mutex:       new(sync.Mutex),
		Names:       make(map[string]string),
	}

	if root, ok := r.db.Root(); ok {
		opts.WorkingDir = root
	}
	return opts
}

// cacheEntry caches then given "from" entry so that it is always rewritten as
// a *TreeEntry equivalent to "to".
func (r *Rewriter) cacheEntry(from, to *odb.TreeEntry) *odb.TreeEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.entries[r.entryKey(from)] = to

	return to
}

// uncacheEntry returns a *TreeEntry that is cached from the given *TreeEntry
// "from". That is to say, it returns the *TreeEntry that "from" should be
// rewritten to, or nil if none could be found.
func (r *Rewriter) uncacheEntry(from *odb.TreeEntry) *odb.TreeEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.entries[r.entryKey(from)]
}

// entryKey returns a unique key for a given *TreeEntry "e".
func (r *Rewriter) entryKey(e *odb.TreeEntry) string {
	return fmt.Sprintf("%s:%x", e.Name, e.Oid)
}

// cacheEntry caches then given "from" commit so that it is always rewritten as
// a *git/odb.Commit equivalent to "to".
func (r *Rewriter) cacheCommit(from, to []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.commits[hex.EncodeToString(from)] = to
}

// uncacheCommit returns a *git/odb.Commit that is cached from the given
// *git/odb.Commit "from". That is to say, it returns the *git/odb.Commit that
// "from" should be rewritten to, or nil if none could be found.
func (r *Rewriter) uncacheCommit(from []byte) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.commits[hex.EncodeToString(from)]
}