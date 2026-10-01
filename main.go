package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/leandergangso/npmbundle/npm"
	"golang.org/x/term"
)

//go:embed publish.sh
var scriptFS embed.FS

const (
	ansiReset  = "\033[0m"
	ansiDim    = "\033[2m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiCyan   = "\033[36m"
)

var (
	program string = "npmbundle" // match go.mod module/repo name
	version string = "dev"       // -X main.version="1.0.0"
)

func main() {
	ctx := context.Background()
	err := run(ctx)
	if err != nil {
		var out = "Error:"
		if useColor() {
			out = ansiRed + "Error:" + ansiReset
		}
		logln(out, err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	now := time.Now()

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	flags, args := parseFlagsAndArgs()

	pkgSpecs, err := extractPkgsFromArgs(args)
	if err != nil {
		return err
	}

	pkgs, err := resolvePackages(ctx, pkgSpecs, flags.Tree, flags.NoDeps)
	if err != nil {
		return err
	}

	if flags.Output != "" {
		logln() // output spacer
		err = createBundle(ctx, pkgs, flags.Output, flags.Sanitize)
		if err != nil {
			return err
		}
	}

	duration := time.Since(now).Seconds()
	if flags.Output == "" {
		logf("\nFinished %d packages without bundling in %.2fs\n", len(pkgs), duration)
	} else {
		logf("\nSuccessfully bundled %d packages in %.2fs\n", len(pkgs), duration)
	}

	return nil
}

func extractPkgsFromArgs(args []string) ([]string, error) {
	var pkgSpecs []string

	for _, input := range args {
		stat, err := os.Stat(input)
		if err != nil {
			if os.IsNotExist(err) {
				pkgSpecs = append(pkgSpecs, input)
				continue
			}
			return nil, fmt.Errorf("failed to access file '%s': %w", input, err)
		}

		if stat.IsDir() {
			return nil, fmt.Errorf("argument '%s' is a directory, only npm pkg or file is allowed", input)
		}

		content, err := os.ReadFile(input)
		if err != nil {
			return nil, fmt.Errorf("failed to read file '%s': %w", input, err)
		}

		for pkgSpec := range strings.SplitSeq(string(content), "\n") {
			pkgSpec = strings.TrimSpace(pkgSpec)
			if pkgSpec == "" || strings.HasPrefix(pkgSpec, "#") {
				continue // empty or comment line
			}
			pkgSpecs = append(pkgSpecs, pkgSpec)
		}
	}

	return pkgSpecs, nil
}

func resolvePackages(ctx context.Context, pkgSpecs []string, printTree bool, noDeps bool) ([]*npm.Package, error) {
	var pkgCache sync.Map
	var visited = make(map[string]struct{})

	var resolveRecursive func(pkgSpec string, depth int) error
	resolveRecursive = func(pkgSpec string, depth int) error {
		if _, ok := visited[pkgSpec]; ok {
			name := pkgSpec
			meta := "[DUPLICATE]"
			if useColor() {
				name = ansiCyan + name + ansiReset
				meta = ansiDim + meta + ansiReset
			}
			if printTree {
				indent := strings.Repeat("  ", depth)
				logf("%s%s %s\n", indent, meta, name)
			} else {
				logf("  %s %s\n", meta, name)
			}
			return nil
		}
		visited[pkgSpec] = struct{}{} // struct{}{} uses 0 memory, it contains no data

		var pkg *npm.Package
		var err error
		var ok bool

		val, ok := pkgCache.Load(pkgSpec)
		if ok {
			pkg, ok = val.(*npm.Package)
			if !ok {
				panic("map should always be of type *npm.Package")
			}
		} else {
			pkg, err = npm.FetchPackageMetadata(ctx, pkgSpec)
			if err != nil {
				logln() // output spacer
				return err
			}
			pkgCache.Store(pkgSpec, pkg)
		}

		name := fmt.Sprintf("%s -> %s", pkgSpec, pkg.Version)
		meta := fmt.Sprintf("| %s • %d files", npm.FormatSize(pkg.Dist.UnpackedSize), pkg.Dist.FileCount)

		if useColor() {
			name = ansiCyan + name + ansiReset
			meta = ansiDim + meta + ansiReset
		}

		if depth == 0 {
			logf("Resolving: %s %s\n", name, meta)
		} else if printTree {
			indent := strings.Repeat("  ", depth)
			logf("%s%s %s\n", indent, name, meta)
		} else {
			logf("  %s %s\n", name, meta)
		}

		allDependencies := make(map[string]string)
		maps.Copy(allDependencies, pkg.Dependencies)
		maps.Copy(allDependencies, pkg.PeerDependencies)
		maps.Copy(allDependencies, pkg.OptionalDependencies)

		if noDeps {
			meta := fmt.Sprintf("[NO-DEPS] ignored %d direct dependencies", len(allDependencies))
			if useColor() {
				meta = ansiDim + meta + ansiReset
			}
			logf(" %s\n", meta)
			return nil
		}

		var wg sync.WaitGroup
		var errOnce sync.Once
		var fetchErr error

		// pre-fetch dependency data concurrently
		for name, constraint := range allDependencies {
			subSpec := npm.ResolveSpec(name, constraint)
			if _, ok = visited[subSpec]; ok {
				continue
			}

			_, ok := pkgCache.Load(subSpec)
			if !ok {
				wg.Go(func() {
					subPkg, err := npm.FetchPackageMetadata(ctx, subSpec)
					if err != nil {
						errOnce.Do(func() { fetchErr = err })
						return
					}
					pkgCache.Store(subSpec, subPkg)
				})
			}
		}

		wg.Wait()

		if fetchErr != nil {
			return fmt.Errorf("failed to fetch pkg: %w", fetchErr)
		}

		// resolve dependencies recursively
		for name, constraint := range allDependencies {
			subSpec := npm.ResolveSpec(name, constraint)
			err = resolveRecursive(subSpec, depth+1)
			if err != nil {
				return err
			}
		}

		return nil
	}

	// initiate recursive dependency lookup
	for _, spec := range pkgSpecs {
		err := resolveRecursive(spec, 0)
		if err != nil {
			return nil, err
		}
	}

	// generate complete pkg list
	var pkgs []*npm.Package
	pkgCache.Range(func(key, value any) bool {
		pkg, ok := value.(*npm.Package)
		if !ok {
			panic("map should always be of type *npm.Package")
		}
		pkgs = append(pkgs, pkg)
		return true
	})

	return pkgs, nil
}

func createBundle(ctx context.Context, pkgs []*npm.Package, output string, sanitize bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// sort pkgs for deterministic behavior
	slices.SortFunc(pkgs, func(a, b *npm.Package) int {
		aName := fmt.Sprintf("%s@%s", a.Name, a.Version)
		bName := fmt.Sprintf("%s@%s", b.Name, b.Version)
		return strings.Compare(aName, bName)
	})

	// create digest from pkgs
	h := sha256.New()
	for _, pkg := range pkgs {
		fmt.Fprintf(h, "%s@%s\n", pkg.Name, pkg.Version)
	}
	digest := hex.EncodeToString(h.Sum(nil)[:12])

	tmpDir := filepath.Join(os.TempDir(), program+"-"+digest)

	var taskC = make(chan *npm.Package, 120)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errCount int

	// worker pool
	const maxWorkers = 12
	for range maxWorkers {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return

				case pkg, ok := <-taskC:
					if !ok {
						return
					}

					size, err := npm.DownloadTarball(ctx, pkg, tmpDir, sanitize)

					mu.Lock()
					if err != nil {
						name := fmt.Sprintf("%s@%s", pkg.Name, pkg.Version)
						meta := fmt.Sprintf("[ERROR: %v]", err)
						if useColor() {
							name = ansiCyan + name + ansiReset
							meta = ansiRed + meta + ansiReset
						}
						logf("Downloading: %s %s\n", meta, name)
						errCount++

					} else if size > 0 {
						name := fmt.Sprintf("%s@%s", pkg.Name, pkg.Version)
						meta := fmt.Sprintf("| %s", npm.FormatSize(size))
						if useColor() {
							name = ansiCyan + name + ansiReset
							meta = ansiDim + meta + ansiReset
						}
						logf("Downloading: %s %s\n", name, meta)

					} else {
						name := fmt.Sprintf("%s@%s", pkg.Name, pkg.Version)
						meta := "[LOCAL]"
						if useColor() {
							name = ansiCyan + name + ansiReset
							meta = ansiDim + meta + ansiReset
						}
						logf("Downloading: %s %s\n", meta, name)
					}
					mu.Unlock()
				}
			}
		})
	}

	// enqueue all packages
	for _, pkg := range pkgs {
		taskC <- pkg
	}
	close(taskC)

	wg.Wait() // wait for worker(s) to complete

	// include script in output
	b, err := scriptFS.ReadFile("publish.sh")
	if err != nil {
		return err
	}
	path := filepath.Join(tmpDir, "publish.sh")
	err = os.WriteFile(path, b, 0755)
	if err != nil {
		return err
	}

	err = archiveDir(tmpDir, output)
	if err != nil {
		return err
	}

	if errCount > 0 {
		logln() // output spacer
		return fmt.Errorf("completed with %d failure(s)", errCount)
	}

	_ = os.RemoveAll(tmpDir)
	return nil
}

func archiveDir(inputDir, outputFile string) error {
	out, err := os.Create(outputFile)
	if err != nil {
		return err
	}
	defer out.Close()

	gz := gzip.NewWriter(out)
	defer gz.Close()

	tw := tar.NewWriter(gz)
	defer tw.Close()

	base := filepath.Base(inputDir)

	entries, err := os.ReadDir(inputDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		path := filepath.Join(inputDir, entry.Name())

		info, err := entry.Info()
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}

		header.Name = filepath.ToSlash(filepath.Join(base, entry.Name()))

		err = tw.WriteHeader(header)
		if err != nil {
			return err
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}

		_, err = io.Copy(tw, f)
		if err != nil {
			f.Close()
			return err
		}

		f.Close()
	}

	return nil
}

var noColor bool

type flags struct {
	Output   string
	Sanitize bool
	NoDeps   bool
	Tree     bool
}

func parseFlagsAndArgs() (flags, []string) {
	fs := flag.NewFlagSet(program, flag.ExitOnError)

	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "%s %s — Bundle npm packages and their entire dependency tree.\n\n", program, version)

		fmt.Fprintf(fs.Output(), "Usage:\n")
		fmt.Fprintf(fs.Output(), "  %s [flags] <pkg... | file...>\n\n", program)

		fmt.Fprintf(fs.Output(), "Examples:\n")
		fmt.Fprintf(fs.Output(), "  %s -tree lodash@4.17.21 lodash@latest\n", program)
		fmt.Fprintf(fs.Output(), "  %s -sanitize ./pkgs.txt\n\n", program)

		fmt.Fprintf(fs.Output(), "Flags:\n")
		fs.PrintDefaults()
	}

	var flags flags
	fs.StringVar(&flags.Output, "output", "npmbundle.tgz", "Output tarball path (use \"\" to skip bundling")
	fs.BoolVar(&flags.Sanitize, "sanitize", false, "Sanitize package.json (removes scripts, repository, etc.)")
	fs.BoolVar(&flags.NoDeps, "no-deps", false, "Skip bundling dependencies")
	fs.BoolVar(&flags.Tree, "tree", false, "Display package hierarchy as a nested tree")
	fs.BoolVar(&noColor, "no-color", false, "Disable colored output")

	_ = fs.Parse(os.Args[1:])

	args := fs.Args()
	if len(args) == 0 {
		if fs.NFlag() == 0 {
			fs.Usage()
			os.Exit(2)
		}
		fmt.Fprintln(fs.Output(), "Error:  require at least one argument")
		os.Exit(1)
	}

	return flags, args
}

func useColor() bool {
	if noColor {
		return false
	}
	if _, exists := os.LookupEnv("NO_COLOR"); exists {
		return false // user preference
	}
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return false // not a tty
	}
	return true
}

func logln(a ...any) {
	fmt.Fprintln(os.Stderr, a...)
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format, a...)
}
