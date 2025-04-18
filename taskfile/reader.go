package taskfile

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/dominikbraun/graph"
	"golang.org/x/sync/errgroup"
	"gopkg.in/yaml.v3"

	"github.com/go-task/task/v3/errors"
	"github.com/go-task/task/v3/internal/env"
	"github.com/go-task/task/v3/internal/filepathext"
	"github.com/go-task/task/v3/internal/templater"
	"github.com/go-task/task/v3/taskfile/ast"
)

const (
	taskfileUntrustedPrompt = `The task you are attempting to run depends on the remote Taskfile at %q.
--- Make sure you trust the source of this Taskfile before continuing ---
Continue?`
	taskfileChangedPrompt = `The Taskfile at %q has changed since you last used it!
--- Make sure you trust the source of this Taskfile before continuing ---
Continue?`
)

type (
	// DebugFunc is a function that can be called to log debug messages.
	DebugFunc func(string)
	// PromptFunc is a function that can be called to prompt the user for input.
	PromptFunc func(string) error
	// A ReaderOption is any type that can apply a configuration to a [Reader].
	ReaderOption interface {
		ApplyToReader(*Reader)
	}
	// A Reader will recursively read Taskfiles from a given [Node] and build a
	// [ast.TaskfileGraph] from them.
	Reader struct {
		graph       *ast.TaskfileGraph
		node        Node
		insecure    bool
		download    bool
		offline     bool
		timeout     time.Duration
		tempDir     string
		debugFunc   DebugFunc
		promptFunc  PromptFunc
		promptMutex sync.Mutex
	}
)

// NewReader constructs a new Taskfile [Reader] using the given Node and
// options.
func NewReader(
	node Node,
	opts ...ReaderOption,
) *Reader {
	r := &Reader{
		graph:       ast.NewTaskfileGraph(),
		node:        node,
		insecure:    false,
		download:    false,
		offline:     false,
		timeout:     time.Second * 10,
		tempDir:     os.TempDir(),
		debugFunc:   nil,
		promptFunc:  nil,
		promptMutex: sync.Mutex{},
	}
	r.Options(opts...)
	return r
}

// Options loops through the given [ReaderOption] functions and applies them to
// the [Reader].
func (r *Reader) Options(opts ...ReaderOption) {
	for _, opt := range opts {
		opt.ApplyToReader(r)
	}
}

// WithInsecure allows the [Reader] to make insecure connections when reading
// remote taskfiles. By default, insecure connections are rejected.
func WithInsecure(insecure bool) ReaderOption {
	return &insecureOption{insecure: insecure}
}

type insecureOption struct {
	insecure bool
}

func (o *insecureOption) ApplyToReader(r *Reader) {
	r.insecure = o.insecure
}

// WithDownload forces the [Reader] to download a fresh copy of the taskfile
// from the remote source.
func WithDownload(download bool) ReaderOption {
	return &downloadOption{download: download}
}

type downloadOption struct {
	download bool
}

func (o *downloadOption) ApplyToReader(r *Reader) {
	r.download = o.download
}

// WithOffline stops the [Reader] from being able to make network connections.
// It will still be able to read local files and cached copies of remote files.
func WithOffline(offline bool) ReaderOption {
	return &offlineOption{offline: offline}
}

type offlineOption struct {
	offline bool
}

func (o *offlineOption) ApplyToReader(r *Reader) {
	r.offline = o.offline
}

// WithTimeout sets the [Reader]'s timeout for fetching remote taskfiles. By
// default, the timeout is set to 10 seconds.
func WithTimeout(timeout time.Duration) ReaderOption {
	return &timeoutOption{timeout: timeout}
}

type timeoutOption struct {
	timeout time.Duration
}

func (o *timeoutOption) ApplyToReader(r *Reader) {
	r.timeout = o.timeout
}

// WithTempDir sets the temporary directory that will be used by the [Reader].
// By default, the reader uses [os.TempDir].
func WithTempDir(tempDir string) ReaderOption {
	return &tempDirOption{tempDir: tempDir}
}

type tempDirOption struct {
	tempDir string
}

func (o *tempDirOption) ApplyToReader(r *Reader) {
	r.tempDir = o.tempDir
}

// WithDebugFunc sets the debug function to be used by the [Reader]. If set,
// this function will be called with debug messages. This can be useful if the
// caller wants to log debug messages from the [Reader]. By default, no debug
// function is set and the logs are not written.
func WithDebugFunc(debugFunc DebugFunc) ReaderOption {
	return &debugFuncOption{debugFunc: debugFunc}
}

type debugFuncOption struct {
	debugFunc DebugFunc
}

func (o *debugFuncOption) ApplyToReader(r *Reader) {
	r.debugFunc = o.debugFunc
}

// WithPromptFunc sets the prompt function to be used by the [Reader]. If set,
// this function will be called with prompt messages. The function should
// optionally log the message to the user and return nil if the prompt is
// accepted and the execution should continue. Otherwise, it should return an
// error which describes why the prompt was rejected. This can then be caught
// and used later when calling the [Reader.Read] method. By default, no prompt
// function is set and all prompts are automatically accepted.
func WithPromptFunc(promptFunc PromptFunc) ReaderOption {
	return &promptFuncOption{promptFunc: promptFunc}
}

type promptFuncOption struct {
	promptFunc PromptFunc
}

func (o *promptFuncOption) ApplyToReader(r *Reader) {
	r.promptFunc = o.promptFunc
}

// Read will read the Taskfile defined by the [Reader]'s [Node] and recurse
// through any [ast.Includes] it finds, reading each included Taskfile and
// building an [ast.TaskfileGraph] as it goes. If any errors occur, they will be
// returned immediately.
func (r *Reader) Read() (*ast.TaskfileGraph, error) {
	if err := r.include(r.node); err != nil {
		return nil, err
	}
	return r.graph, nil
}

func (r *Reader) debugf(format string, a ...any) {
	if r.debugFunc != nil {
		r.debugFunc(fmt.Sprintf(format, a...))
	}
}

func (r *Reader) promptf(format string, a ...any) error {
	if r.promptFunc != nil {
		return r.promptFunc(fmt.Sprintf(format, a...))
	}
	return nil
}

func (r *Reader) include(node Node) error {
	// Create a new vertex for the Taskfile
	vertex := &ast.TaskfileVertex{
		URI:      node.Location(),
		Taskfile: nil,
	}

	// Add the included Taskfile to the DAG
	// If the vertex already exists, we return early since its Taskfile has
	// already been read and its children explored
	if err := r.graph.AddVertex(vertex); err == graph.ErrVertexAlreadyExists {
		return nil
	} else if err != nil {
		return err
	}

	// Read and parse the Taskfile from the file and add it to the vertex
	var err error
	vertex.Taskfile, err = r.readNode(node)
	if err != nil {
		return err
	}

	// Create an error group to wait for all included Taskfiles to be read
	var g errgroup.Group

	// Loop over each included taskfile
	for _, include := range vertex.Taskfile.Includes.All() {
		vars := env.GetEnviron()
		vars.Merge(vertex.Taskfile.Vars, nil)
		// Start a goroutine to process each included Taskfile
		g.Go(func() error {
			cache := &templater.Cache{Vars: vars}
			include = &ast.Include{
				Namespace:      include.Namespace,
				Taskfile:       templater.Replace(include.Taskfile, cache),
				Dir:            templater.Replace(include.Dir, cache),
				Optional:       include.Optional,
				Internal:       include.Internal,
				Flatten:        include.Flatten,
				Aliases:        include.Aliases,
				AdvancedImport: include.AdvancedImport,
				Excludes:       include.Excludes,
				Vars:           templater.ReplaceVars(include.Vars, cache),
			}
			if err := cache.Err(); err != nil {
				return err
			}

			entrypoint, err := node.ResolveEntrypoint(include.Taskfile)
			if err != nil {
				return err
			}

			include.Dir, err = node.ResolveDir(include.Dir)
			if err != nil {
				return err
			}

			includeNode, err := NewNode(entrypoint, include.Dir, r.insecure, r.timeout,
				WithParent(node),
			)
			if err != nil {
				if include.Optional {
					return nil
				}
				return err
			}

			// Recurse into the included Taskfile
			if err := r.include(includeNode); err != nil {
				return err
			}

			// Create an edge between the Taskfiles
			r.graph.Lock()
			defer r.graph.Unlock()
			edge, err := r.graph.Edge(node.Location(), includeNode.Location())
			if err == graph.ErrEdgeNotFound {
				// If the edge doesn't exist, create it
				err = r.graph.AddEdge(
					node.Location(),
					includeNode.Location(),
					graph.EdgeData([]*ast.Include{include}),
					graph.EdgeWeight(1),
				)
			} else {
				// If the edge already exists
				edgeData := append(edge.Properties.Data.([]*ast.Include), include)
				err = r.graph.UpdateEdge(
					node.Location(),
					includeNode.Location(),
					graph.EdgeData(edgeData),
					graph.EdgeWeight(len(edgeData)),
				)
			}
			if errors.Is(err, graph.ErrEdgeCreatesCycle) {
				return errors.TaskfileCycleError{
					Source:      node.Location(),
					Destination: includeNode.Location(),
				}
			}
			return err
		})
	}

	// Wait for all the go routines to finish
	return g.Wait()
}

func (r *Reader) readNode(node Node) (*ast.Taskfile, error) {
	b, err := r.loadNodeContent(node)
	if err != nil {
		return nil, err
	}

	var tf ast.Taskfile
	if err := yaml.Unmarshal(b, &tf); err != nil {
		// Decode the taskfile and add the file info the any errors
		taskfileDecodeErr := &errors.TaskfileDecodeError{}
		if errors.As(err, &taskfileDecodeErr) {
			snippet := NewSnippet(b,
				WithLine(taskfileDecodeErr.Line),
				WithColumn(taskfileDecodeErr.Column),
				WithPadding(2),
			)
			return nil, taskfileDecodeErr.WithFileInfo(node.Location(), snippet.String())
		}
		return nil, &errors.TaskfileInvalidError{URI: filepathext.TryAbsToRel(node.Location()), Err: err}
	}

	// Check that the Taskfile is set and has a schema version
	if tf.Version == nil {
		return nil, &errors.TaskfileVersionCheckError{URI: node.Location()}
	}

	// Set the taskfile/task's locations
	tf.Location = node.Location()
	for task := range tf.Tasks.Values(nil) {
		// If the task is not defined, create a new one
		if task == nil {
			task = &ast.Task{}
		}
		// Set the location of the taskfile for each task
		if task.Location.Taskfile == "" {
			task.Location.Taskfile = tf.Location
		}
	}

	return &tf, nil
}

func (r *Reader) loadNodeContent(node Node) ([]byte, error) {
	if !node.Remote() {
		ctx, cf := context.WithTimeout(context.Background(), r.timeout)
		defer cf()
		return node.Read(ctx)
	}

	cache, err := NewCache(r.tempDir)
	if err != nil {
		return nil, err
	}

	if r.offline {
		// In offline mode try to use cached copy
		cached, err := cache.read(node)
		if errors.Is(err, os.ErrNotExist) {
			return nil, &errors.TaskfileCacheNotFoundError{URI: node.Location()}
		} else if err != nil {
			return nil, err
		}
		r.debugf("task: [%s] Fetched cached copy\n", node.Location())

		return cached, nil
	}

	ctx, cf := context.WithTimeout(context.Background(), r.timeout)
	defer cf()

	b, err := node.Read(ctx)
	if errors.Is(err, &errors.TaskfileNetworkTimeoutError{}) {
		// If we timed out then we likely have a network issue

		// If a download was requested, then we can't use a cached copy
		if r.download {
			return nil, &errors.TaskfileNetworkTimeoutError{URI: node.Location(), Timeout: r.timeout}
		}

		// Search for any cached copies
		cached, err := cache.read(node)
		if errors.Is(err, os.ErrNotExist) {
			return nil, &errors.TaskfileNetworkTimeoutError{URI: node.Location(), Timeout: r.timeout, CheckedCache: true}
		} else if err != nil {
			return nil, err
		}
		r.debugf("task: [%s] Network timeout. Fetched cached copy\n", node.Location())

		return cached, nil

	} else if err != nil {
		return nil, err
	}
	r.debugf("task: [%s] Fetched remote copy\n", node.Location())

	// Get the checksums
	checksum := checksum(b)
	cachedChecksum := cache.readChecksum(node)

	var prompt string
	if cachedChecksum == "" {
		// If the checksum doesn't exist, prompt the user to continue
		prompt = taskfileUntrustedPrompt
	} else if checksum != cachedChecksum {
		// If there is a cached hash, but it doesn't match the expected hash, prompt the user to continue
		prompt = taskfileChangedPrompt
	}

	if prompt != "" {
		if err := func() error {
			r.promptMutex.Lock()
			defer r.promptMutex.Unlock()
			return r.promptf(prompt, node.Location())
		}(); err != nil {
			return nil, &errors.TaskfileNotTrustedError{URI: node.Location()}
		}

		// Store the checksum
		if err := cache.writeChecksum(node, checksum); err != nil {
			return nil, err
		}

		// Cache the file
		r.debugf("task: [%s] Caching downloaded file\n", node.Location())
		if err = cache.write(node, b); err != nil {
			return nil, err
		}
	}

	return b, nil
}
