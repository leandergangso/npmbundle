package main

import (
	"context"
	"flag"
	"fmt"
	"maps"
	"npmpack/npm"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

const (
	ansiReset  = "\033[0m"
	ansiDim    = "\033[2m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiCyan   = "\033[36m"
)

var (
	program string = "npmpack" // match go.mod module
	version string = "dev"     // -X main.version="1.0.0"
)

func main() {
	ctx := context.Background()
	err := run(ctx)
	if err != nil {
		var out = "Error:"
		if useColor() {
			out = ansiRed + "Error:" + ansiReset
		}
		logln(out, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	now := time.Now()

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	args := parseFlagsAndArgs()

	pkgSpecs, err := extractPkgsFromArgs(args)
	if err != nil {
		return err
	}

	pkgs, err := resolvePackages(ctx, pkgSpecs)
	if err != nil {
		return err
	}

	if flagOutputDir != "" {
		logln() // output spacer
		err = downloadTarballs(ctx, pkgs)
		if err != nil {
			return err
		}
	}

	duration := time.Since(now).Seconds()
	if flagOutputDir == "" {
		logf("\nFinished preview in %.2fs\n", duration)
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

func resolvePackages(ctx context.Context, pkgSpecs []string) ([]*npm.Package, error) {
	var mu sync.Mutex
	var pkgs []*npm.Package
	visited := make(map[string]struct{})
	pkgCache := make(map[string]*npm.Package)

	var resolveFn func(pkgSpec string, depth int) error
	resolveFn = func(pkgSpec string, depth int) error {
		if _, ok := visited[pkgSpec]; ok {
			nameVer := pkgSpec
			meta := "[DUPLICATE]"
			if useColor() {
				nameVer = ansiCyan + nameVer + ansiReset
				meta = ansiDim + meta + ansiReset
			}
			if flagTree {
				indent := strings.Repeat("  ", depth)
				logf("%s%s %s\n", indent, meta, nameVer)
			} else {
				logf("  %s %s\n", meta, nameVer)
			}
			return nil
		}
		visited[pkgSpec] = struct{}{} // struct{}{} uses 0 memory, it contains no data

		var pkg *npm.Package
		var err error

		mu.Lock()
		pkg, ok := pkgCache[pkgSpec]
		mu.Unlock()

		if !ok {
			pkg, err = npm.FetchPackageMetadata(ctx, pkgSpec)
			if err != nil {
				logln() // output spacer
				return err
			}
			mu.Lock()
			pkgCache[pkgSpec] = pkg
			mu.Unlock()
		}

		pkgs = append(pkgs, pkg)

		nameVer := fmt.Sprintf("%s@%s", pkg.Name, pkg.Version)
		meta := fmt.Sprintf("[%s - %d files]", npm.FormatSize(pkg.Dist.UnpackedSize), pkg.Dist.FileCount)

		if useColor() {
			nameVer = ansiCyan + nameVer + ansiReset
			meta = ansiDim + meta + ansiReset
		}

		if depth == 0 {
			logf("Resolving: %s %s\n", nameVer, meta)
		} else if flagTree {
			indent := strings.Repeat("  ", depth)
			logf("%s%s %s\n", indent, nameVer, meta)
		} else {
			logf("  %s %s\n", nameVer, meta)
		}

		allDependencies := make(map[string]string)
		maps.Copy(allDependencies, pkg.Dependencies)
		maps.Copy(allDependencies, pkg.PeerDependencies)
		maps.Copy(allDependencies, pkg.OptionalDependencies)

		var wg sync.WaitGroup
		var errOnce sync.Once
		var fetchErr error

		for name, constraint := range allDependencies {
			subSpec := fmt.Sprintf("%s@%s", name, constraint)
			if _, ok = visited[subSpec]; ok {
				continue
			}

			wg.Go(func() {
				mu.Lock()
				_, ok := pkgCache[subSpec]
				mu.Unlock()

				if !ok {
					childPkg, err := npm.FetchPackageMetadata(ctx, subSpec)
					if err != nil {
						errOnce.Do(func() { fetchErr = err })
						return
					}
					mu.Lock()
					pkgCache[subSpec] = childPkg
					mu.Unlock()
				}
			})
		}

		wg.Wait()

		if fetchErr != nil {
			return fetchErr
		}

		for name, constraint := range allDependencies {
			subSpec := fmt.Sprintf("%s@%s", name, constraint)
			err = resolveFn(subSpec, depth+1)
			if err != nil {
				return err
			}
		}

		return nil
	}

	for _, spec := range pkgSpecs {
		err := resolveFn(spec, 0)
		if err != nil {
			return nil, err
		}
	}

	return pkgs, nil
}

func downloadTarballs(ctx context.Context, pkgs []*npm.Package) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var taskC = make(chan *npm.Package, len(pkgs))
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

					size, err := npm.DownloadTarball(ctx, pkg, flagOutputDir, flagSanitize)

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
						meta := fmt.Sprintf("[%s]", npm.FormatSize(size))
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

	wg.Wait() // wait for worker to complete

	if errCount > 0 {
		logln() // output spacer
		return fmt.Errorf("completed with %d failure(s)", errCount)
	}

	return nil
}

// flags
var (
	flagOutputDir string
	flagSanitize  bool
	flagNoColor   bool
	flagTree      bool
)

func parseFlagsAndArgs() []string {
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

	fs.StringVar(&flagOutputDir, "output", "./npmpkgs", "Target directory for tarballs (set to \"\" for preview mode)")
	fs.BoolVar(&flagSanitize, "sanitize", false, "Sanitize package.json (removes scripts, repository, etc.)")
	fs.BoolVar(&flagNoColor, "no-color", false, "Disable colored output")
	fs.BoolVar(&flagTree, "tree", false, "Display package hierarchy as a nested tree")

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

	return args
}

func useColor() bool {
	if flagNoColor {
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
