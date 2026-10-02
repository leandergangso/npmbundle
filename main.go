package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/leandergangso/npmbundle/npm"
	"golang.org/x/term"
)

//go:embed publish.sh
var publishScript []byte

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
		logf("\nResolved %d packages without bundling in %.2fs\n", len(pkgs), duration)
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
	var pkgs []*npm.Package
	var pkgCache sync.Map
	var visitedResolved = make(map[string]struct{})

	var doneCount int
	total := len(pkgSpecs)
	width := len(strconv.Itoa(total))

	var resolveDependencies func(pkgSpec string, depth int) error
	resolveDependencies = func(pkgSpec string, depth int) error {
		var pkg *npm.Package
		var err error

		if val, ok := pkgCache.Load(pkgSpec); ok {
			pkg = val.(*npm.Package)
		} else {
			pkg, err = npm.FetchPackageMetadata(ctx, pkgSpec)
			if err != nil {
				logln() // output spacer
				return err
			}
			pkgCache.Store(pkgSpec, pkg)
			resolvedSpec := pkg.Name + "@" + pkg.Version
			if pkgSpec != resolvedSpec {
				pkgCache.Store(resolvedSpec, pkg)
			}
		}

		resolvedSpec := pkg.Name + "@" + pkg.Version
		if _, ok := visitedResolved[resolvedSpec]; ok {
			if depth == 0 {
				doneCount++
				name := fmt.Sprintf("%s -> %s", pkgSpec, pkg.Version)
				meta := "[DUPLICATE]"
				countMeta := fmt.Sprintf("[%*d/%d]", width, doneCount, total)
				if useColor() {
					name = ansiCyan + name + ansiReset
					meta = ansiDim + meta + ansiReset
					countMeta = ansiDim + countMeta + ansiReset
				}
				logf("%s Resolving: %s %s\n", countMeta, meta, name)
				return nil
			}
			name := fmt.Sprintf("%s -> %s", pkgSpec, pkg.Version)
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
		visitedResolved[resolvedSpec] = struct{}{}
		pkgs = append(pkgs, pkg)

		name := fmt.Sprintf("%s -> %s", pkgSpec, pkg.Version)
		meta := fmt.Sprintf("| %s • %d files", npm.FormatSize(pkg.Dist.UnpackedSize), pkg.Dist.FileCount)

		if useColor() {
			name = ansiCyan + name + ansiReset
			meta = ansiDim + meta + ansiReset
		}

		if depth == 0 {
			doneCount++
			countMeta := fmt.Sprintf("[%*d/%d]", width, doneCount, total)
			if useColor() {
				countMeta = ansiDim + countMeta + ansiReset
			}
			logf("%s Resolving: %s %s\n", countMeta, name, meta)
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

			if _, ok := pkgCache.Load(subSpec); !ok {
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

		for name, constraint := range allDependencies {
			subSpec := npm.ResolveSpec(name, constraint)
			err = resolveDependencies(subSpec, depth+1)
			if err != nil {
				return err
			}
		}

		return nil
	}

	for _, spec := range pkgSpecs {
		err := resolveDependencies(spec, 0)
		if err != nil {
			return nil, err
		}
	}

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

	// create digest
	h := sha256.New()
	fmt.Fprintf(h, "sanitize:%v\n", sanitize)
	for _, pkg := range pkgs {
		fmt.Fprintf(h, "package:%s@%s\n", pkg.Name, pkg.Version)
	}
	digest := hex.EncodeToString(h.Sum(nil)[:12])

	tmpDir := filepath.Join(os.TempDir(), program+"-"+digest)

	err := os.MkdirAll(tmpDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create output directory %s: %w", tmpDir, err)
	}

	var taskC = make(chan *npm.Package, 64)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errCount int
	var doneCount int

	total := len(pkgs)
	width := len(strconv.Itoa(total))

	// worker pool
	const maxWorkers = 16
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
					doneCount++
					countMeta := fmt.Sprintf("[%*d/%d]", width, doneCount, total)
					if useColor() {
						countMeta = ansiDim + countMeta + ansiReset
					}

					if err != nil {
						name := fmt.Sprintf("%s@%s", pkg.Name, pkg.Version)
						meta := fmt.Sprintf("[ERROR: %v]", err)
						if useColor() {
							name = ansiCyan + name + ansiReset
							meta = ansiRed + meta + ansiReset
						}
						logf("%s Downloading: %s %s\n", countMeta, meta, name)
						errCount++

					} else if size > 0 {
						name := fmt.Sprintf("%s@%s", pkg.Name, pkg.Version)
						meta := fmt.Sprintf("| %s", npm.FormatSize(size))
						if useColor() {
							name = ansiCyan + name + ansiReset
							meta = ansiDim + meta + ansiReset
						}
						logf("%s Downloading: %s %s\n", countMeta, name, meta)

					} else {
						name := fmt.Sprintf("%s@%s", pkg.Name, pkg.Version)
						meta := "[LOCAL]"
						if useColor() {
							name = ansiCyan + name + ansiReset
							meta = ansiDim + meta + ansiReset
						}
						logf("%s Downloading: %s %s\n", countMeta, meta, name)
					}

					mu.Unlock()
				}
			}
		})
	}

	// enqueue all packages
	for _, pkg := range pkgs {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			taskC <- pkg
		}
	}
	close(taskC)

	wg.Wait() // wait for worker(s) to complete

	// include script in output
	path := filepath.Join(tmpDir, "publish.sh")
	err = os.WriteFile(path, publishScript, 0755)
	if err != nil {
		return err
	}

	logf("\nBuilding %s archive...\n", output)
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
