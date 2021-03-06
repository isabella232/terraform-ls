package rootmodule

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gammazero/workerpool"
	"github.com/hashicorp/hcl-lang/schema"
	"github.com/hashicorp/terraform-ls/internal/filesystem"
	"github.com/hashicorp/terraform-ls/internal/terraform/discovery"
	"github.com/hashicorp/terraform-ls/internal/terraform/exec"
)

type rootModuleManager struct {
	rms           []*rootModule
	newRootModule RootModuleFactory
	filesystem    filesystem.Filesystem

	syncLoading bool
	workerPool  *workerpool.WorkerPool
	logger      *log.Logger

	// terraform discovery
	tfDiscoFunc discovery.DiscoveryFunc

	// terraform executor
	tfNewExecutor exec.ExecutorFactory
	tfExecPath    string
	tfExecTimeout time.Duration
	tfExecLogPath string
}

func NewRootModuleManager(fs filesystem.Filesystem) RootModuleManager {
	return newRootModuleManager(fs)
}

func newRootModuleManager(fs filesystem.Filesystem) *rootModuleManager {
	d := &discovery.Discovery{}

	defaultSize := 3 * runtime.NumCPU()
	wp := workerpool.New(defaultSize)

	rmm := &rootModuleManager{
		rms:           make([]*rootModule, 0),
		filesystem:    fs,
		workerPool:    wp,
		logger:        defaultLogger,
		tfDiscoFunc:   d.LookPath,
		tfNewExecutor: exec.NewExecutor,
	}
	rmm.newRootModule = rmm.defaultRootModuleFactory
	return rmm
}

func (rmm *rootModuleManager) WorkerPoolSize() int {
	return rmm.workerPool.Size()
}

func (rmm *rootModuleManager) WorkerQueueSize() int {
	return rmm.workerPool.WaitingQueueSize()
}

func (rmm *rootModuleManager) defaultRootModuleFactory(ctx context.Context, dir string) (*rootModule, error) {
	rm := newRootModule(rmm.filesystem, dir)

	rm.SetLogger(rmm.logger)

	d := &discovery.Discovery{}
	rm.tfDiscoFunc = d.LookPath
	rm.tfNewExecutor = exec.NewExecutor

	rm.tfExecPath = rmm.tfExecPath
	rm.tfExecTimeout = rmm.tfExecTimeout
	rm.tfExecLogPath = rmm.tfExecLogPath

	return rm, rm.discoverCaches(ctx, dir)
}

func (rmm *rootModuleManager) SetTerraformExecPath(path string) {
	rmm.tfExecPath = path
}

func (rmm *rootModuleManager) SetTerraformExecLogPath(logPath string) {
	rmm.tfExecLogPath = logPath
}

func (rmm *rootModuleManager) SetTerraformExecTimeout(timeout time.Duration) {
	rmm.tfExecTimeout = timeout
}

func (rmm *rootModuleManager) SetLogger(logger *log.Logger) {
	rmm.logger = logger
}

func (rmm *rootModuleManager) InitAndUpdateRootModule(ctx context.Context, dir string) (RootModule, error) {
	rm, err := rmm.RootModuleByPath(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to get root module: %+v", err)
	}

	if err := rm.ExecuteTerraformInit(ctx); err != nil {
		return nil, fmt.Errorf("failed to init root module: %+v", err)
	}

	rootModule := rm.(*rootModule)
	rootModule.discoverCaches(ctx, dir)
	return rm, rootModule.UpdateProviderSchemaCache(ctx, rootModule.pluginLockFile)
}

func (rmm *rootModuleManager) AddAndStartLoadingRootModule(ctx context.Context, dir string) (RootModule, error) {
	dir = filepath.Clean(dir)

	// TODO: Follow symlinks (requires proper test data)

	if _, ok := rmm.rootModuleByPath(dir); ok {
		return nil, fmt.Errorf("root module %s was already added", dir)
	}

	rm, err := rmm.newRootModule(context.Background(), dir)
	if err != nil {
		return nil, err
	}

	rmm.rms = append(rmm.rms, rm)

	if rmm.syncLoading {
		rmm.logger.Printf("synchronously loading root module %s", dir)
		return rm, rm.load(ctx)
	}

	rmm.logger.Printf("asynchronously loading root module %s", dir)
	rmm.workerPool.Submit(func() {
		rm := rm
		err := rm.load(context.Background())
		rm.setLoadErr(err)
	})

	return rm, nil
}

func (rmm *rootModuleManager) SchemaForPath(path string) (*schema.BodySchema, error) {
	candidates := rmm.RootModuleCandidatesByPath(path)
	for _, rm := range candidates {
		schema, err := rm.MergedSchema()
		if err != nil {
			rmm.logger.Printf("failed to merge schema for %s: %s", rm.Path(), err)
			continue
		}
		if schema != nil {
			rmm.logger.Printf("found schema for %s at %s", path, rm.Path())
			return schema, nil
		}
	}

	rm, err := rmm.RootModuleByPath(path)
	if err != nil {
		return nil, err
	}

	return rm.MergedSchema()
}

func (rmm *rootModuleManager) rootModuleByPath(dir string) (*rootModule, bool) {
	for _, rm := range rmm.rms {
		if pathEquals(rm.Path(), dir) {
			return rm, true
		}
	}
	return nil, false
}

// RootModuleCandidatesByPath finds any initialized root modules
func (rmm *rootModuleManager) RootModuleCandidatesByPath(path string) RootModules {
	path = filepath.Clean(path)

	candidates := make([]RootModule, 0)

	// TODO: Follow symlinks (requires proper test data)

	rm, foundPath := rmm.rootModuleByPath(path)
	if foundPath {
		inited, _ := rm.WasInitialized()
		if inited {
			candidates = append(candidates, rm)
		}
	}

	if !foundPath {
		dir := trimLockFilePath(path)
		rm, ok := rmm.rootModuleByPath(dir)
		if ok {
			inited, _ := rm.WasInitialized()
			if inited {
				candidates = append(candidates, rm)
			}
		}
	}

	for _, rm := range rmm.rms {
		if rm.ReferencesModulePath(path) {
			candidates = append(candidates, rm)
		}
	}

	return candidates
}

func (rmm *rootModuleManager) ListRootModules() RootModules {
	modules := make([]RootModule, 0)
	for _, rm := range rmm.rms {
		modules = append(modules, rm)
	}
	return modules
}
func (rmm *rootModuleManager) RootModuleByPath(path string) (RootModule, error) {
	path = filepath.Clean(path)

	if rm, ok := rmm.rootModuleByPath(path); ok {
		return rm, nil
	}

	dir := trimLockFilePath(path)

	if rm, ok := rmm.rootModuleByPath(dir); ok {
		return rm, nil
	}

	return nil, &RootModuleNotFoundErr{path}
}

func (rmm *rootModuleManager) IsProviderSchemaLoaded(path string) (bool, error) {
	rm, err := rmm.RootModuleByPath(path)
	if err != nil {
		return false, err
	}

	return rm.IsProviderSchemaLoaded(), nil
}

func (rmm *rootModuleManager) TerraformFormatterForDir(ctx context.Context, path string) (exec.Formatter, error) {
	rm, err := rmm.RootModuleByPath(path)
	if err != nil {
		if IsRootModuleNotFound(err) {
			return rmm.newTerraformFormatter(ctx, path)
		}
		return nil, err
	}

	return rm.TerraformFormatter()
}

func (rmm *rootModuleManager) newTerraformFormatter(ctx context.Context, workDir string) (exec.Formatter, error) {
	tfPath := rmm.tfExecPath
	if tfPath == "" {
		var err error
		tfPath, err = rmm.tfDiscoFunc()
		if err != nil {
			return nil, err
		}
	}

	tf, err := rmm.tfNewExecutor(workDir, tfPath)
	if err != nil {
		return nil, err
	}

	tf.SetLogger(rmm.logger)

	if rmm.tfExecLogPath != "" {
		tf.SetExecLogPath(rmm.tfExecLogPath)
	}

	if rmm.tfExecTimeout != 0 {
		tf.SetTimeout(rmm.tfExecTimeout)
	}

	version, _, err := tf.Version(ctx)
	if err != nil {
		return nil, err
	}
	rmm.logger.Printf("Terraform version %s found at %s (alternative)", version, tf.GetExecPath())

	return tf.Format, nil
}

func (rmm *rootModuleManager) IsTerraformAvailable(path string) (bool, error) {
	rm, err := rmm.RootModuleByPath(path)
	if err != nil {
		return false, err
	}

	return rm.IsTerraformAvailable(), nil
}

func (rmm *rootModuleManager) HasTerraformDiscoveryFinished(path string) (bool, error) {
	rm, err := rmm.RootModuleByPath(path)
	if err != nil {
		return false, err
	}

	return rm.HasTerraformDiscoveryFinished(), nil
}

func (rmm *rootModuleManager) CancelLoading() {
	for _, rm := range rmm.rms {
		rmm.logger.Printf("cancelling loading for %s", rm.Path())
		rm.CancelLoading()
		rmm.logger.Printf("loading cancelled for %s", rm.Path())
	}
	rmm.workerPool.Stop()
}

// rootModuleDirFromPath strips known lock file paths and filenames
// to get the directory path of the relevant rootModule
func trimLockFilePath(filePath string) string {
	pluginLockFileSuffixes := pluginLockFilePaths(string(os.PathSeparator))
	for _, s := range pluginLockFileSuffixes {
		if strings.HasSuffix(filePath, s) {
			return strings.TrimSuffix(filePath, s)
		}
	}

	moduleManifestSuffix := moduleManifestFilePath(string(os.PathSeparator))
	if strings.HasSuffix(filePath, moduleManifestSuffix) {
		return strings.TrimSuffix(filePath, moduleManifestSuffix)
	}

	return filePath
}

func (rmm *rootModuleManager) PathsToWatch() []string {
	paths := make([]string, 0)
	for _, rm := range rmm.rms {
		ptw := rm.PathsToWatch()
		if len(ptw) > 0 {
			paths = append(paths, ptw...)
		}
	}
	return paths
}

// NewRootModuleLoader allows adding & loading root modules
// with a given context. This can be passed down to any handler
// which itself will have short-lived context
// therefore couldn't finish loading the root module asynchronously
// after it responds to the client
func NewRootModuleLoader(ctx context.Context, rmm RootModuleManager) RootModuleLoader {
	return func(dir string) (RootModule, error) {
		return rmm.AddAndStartLoadingRootModule(ctx, dir)
	}
}
