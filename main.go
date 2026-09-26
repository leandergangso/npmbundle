package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	"golang.org/x/term"
)

const (
	ansiReset = "\033[0m"
	ansiDim   = "\033[2m"
	ansiCyan  = "\033[36m"
)

var (
	program string = "npmpack" // should match go.mod module
	version string = "dev"     // -X main.version="1.0.0"

	fileMu   sync.Mutex
	seenMu   sync.Mutex
	printMu  sync.Mutex
	seenPkgs = make(map[string]struct{})

	flagOutputDir  string
	flagConcurrent bool
	flagIncludeDev bool
	flagSanitize   bool
	flagNoColor    bool
	flagUseTree    bool
)

type NpmPackage struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	License              string            `json:"license"`
	Dist                 NpmDist           `json:"dist"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

type NpmDist struct {
	Tarball      string `json:"tarball"`
	Integrity    string `json:"integrity"`
	Shasum       string `json:"shasum"`
	UnpackedSize int64  `json:"unpackedSize"`
}

// main is the entry point of the application.
func main() {
	now := time.Now()
	ctx := context.Background()
	err := run(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "\nErrors:\n  -", err)
		os.Exit(1)
	}

	if flagOutputDir == "" {
		fmt.Fprintf(os.Stderr, "\nFinished in %.2fs\n", time.Since(now).Seconds())
	} else {
		count := len(seenPkgs)
		fmt.Fprintf(os.Stderr, "\nSuccessfully bundled %d packages in %.2fs\n", count, time.Since(now).Seconds())
	}
}

// run parses command-line flags and dispatches execution to file or package handlers.
func run(ctx context.Context) error {
	fs := flag.NewFlagSet(program, flag.ExitOnError)

	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "%s %s — Bundle npm packages and their entire dependency tree.\n\n", program, version)

		fmt.Fprintf(fs.Output(), "Usage:\n")
		fmt.Fprintf(fs.Output(), "  %s [flags] <packages.txt... | pkg@version...>\n\n", program)

		fmt.Fprintf(fs.Output(), "Examples:\n")
		fmt.Fprintf(fs.Output(), "  %s -tree lodash@4.17.21\n", program)
		fmt.Fprintf(fs.Output(), "  %s -sanitize ./packages.txt\n\n", program)

		fmt.Fprintf(fs.Output(), "Flags:\n")
		fs.PrintDefaults()
	}

	fs.StringVar(&flagOutputDir, "output", "./npmpkgs", "Target directory for tarballs (set to \"\" for preview mode)")
	fs.BoolVar(&flagConcurrent, "concurrent", false, "Enable concurrent package downloads")
	fs.BoolVar(&flagIncludeDev, "dev", false, "Include devDependencies. Warning: this will take a long time to bundle!")
	fs.BoolVar(&flagSanitize, "sanitize", false, "Sanitize package.json (removes scripts, repository, etc.)")
	fs.BoolVar(&flagNoColor, "no-color", false, "Disable colored output")
	fs.BoolVar(&flagUseTree, "tree", false, "Display package hierarchy as a nested tree")

	_ = fs.Parse(os.Args[1:])

	args := fs.Args()
	if len(args) == 0 {
		fs.Usage()
		os.Exit(2)
	}

	if flagIncludeDev && flagConcurrent {
		flagConcurrent = false
		fmt.Fprintln(os.Stderr, "NOTICE: concurrency is disabled when using -dev")
	}
	if flagUseTree && flagConcurrent {
		flagConcurrent = false
		fmt.Fprintln(os.Stderr, "NOTICE: concurrency is disabled when using -tree")
	}

	var errs error
	for _, input := range args {
		fi, err := os.Stat(input)
		if err == nil {
			if fi.IsDir() {
				errs = errors.Join(errs, fmt.Errorf("argument cannot be a directory: %s", input))
				continue
			}
			err = handleFile(ctx, input)
			if err != nil {
				errs = errors.Join(errs, err)
			}
		} else {
			err = handleNpmPkg(ctx, input, 0)
			if err != nil {
				errs = errors.Join(errs, err)
			}
		}
	}

	return errs
}

// handleFile reads a plain text package list file line by line.
func handleFile(ctx context.Context, filePath string) error {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	var errs error
	for pkgStr := range strings.SplitSeq(string(content), "\n") {
		pkgStr = strings.TrimSpace(pkgStr)
		if pkgStr == "" || strings.HasPrefix(pkgStr, "#") {
			continue // empty or comment line
		}
		err = handleNpmPkg(ctx, pkgStr, 0)
		if err != nil {
			errs = errors.Join(errs, err)
		}
	}

	return errs
}

// handleNpmPkg fetches metadata for a package spec and delegates to dependency processing.
func handleNpmPkg(ctx context.Context, pkgStr string, depth int) error {
	seenMu.Lock()
	if _, ok := seenPkgs[pkgStr]; ok {
		seenMu.Unlock()
		return nil
	}
	seenPkgs[pkgStr] = struct{}{}
	seenMu.Unlock()

	outByte, err := fetchPackageMetadata(ctx, pkgStr)
	if err != nil {
		return err
	}

	var pkg NpmPackage
	err = json.Unmarshal(outByte, &pkg)
	if err != nil {
		return fmt.Errorf("failed to parse package '%s' metadata: %w", pkgStr, err)
	}

	if pkg.Name == "" || pkg.Version == "" {
		return nil // required fields missing, skip
	}

	license := pkg.License
	if license == "" {
		license = "N/A"
	}

	nameVer := fmt.Sprintf("%s@%s", pkg.Name, pkg.Version)
	meta := fmt.Sprintf("(%s) [%s]", license, formatBytes(pkg.Dist.UnpackedSize))

	if useColor() {
		nameVer = ansiCyan + nameVer + ansiReset
		meta = ansiDim + meta + ansiReset
	}

	printMu.Lock()
	if depth == 0 {
		fmt.Printf("processing: %s %s\n", nameVer, meta)
	} else if flagUseTree {
		indent := strings.Repeat("  ", depth)
		fmt.Printf("%s%s %s\n", indent, nameVer, meta)
	} else {
		fmt.Printf("  %s %s\n", nameVer, meta)
	}
	printMu.Unlock()

	return handleDeps(ctx, pkg, depth)
}

// handleDeps outputs the tarball and recursively processes all package dependencies.
func handleDeps(ctx context.Context, pkg NpmPackage, depth int) error {
	if flagOutputDir != "" {
		fileMu.Lock()

		outPath, err := fetchOutputTarball(pkg, flagOutputDir)
		if err != nil {
			fileMu.Unlock()
			return fmt.Errorf("failed to generate tarball for %s: %w", pkg.Name+"@"+pkg.Version, err)
		}

		if flagSanitize {
			tmpPath := outPath + ".tmp"
			err = sanitizeTarball(outPath, tmpPath)
			if err != nil {
				os.Remove(tmpPath)
				fileMu.Unlock()
				return fmt.Errorf("failed to sanitize package.json '%s':  %w", outPath, err)
			}
			err = os.Rename(tmpPath, outPath)
			if err != nil {
				os.Remove(tmpPath)
				fileMu.Unlock()
				return err
			}
		}

		fileMu.Unlock()
	}

	allDependencies := make(map[string]string)
	maps.Copy(allDependencies, pkg.Dependencies)
	maps.Copy(allDependencies, pkg.PeerDependencies)
	maps.Copy(allDependencies, pkg.OptionalDependencies)

	if flagIncludeDev {
		// WARN: this will take a long time to bundle
		maps.Copy(allDependencies, pkg.DevDependencies)
	}

	if flagConcurrent {
		var wg sync.WaitGroup
		var errMu sync.Mutex
		var errs error

		for name, version := range allDependencies {
			pkgStr := fmt.Sprintf("%s@%s", name, version)
			wg.Go(func() {
				err := handleNpmPkg(ctx, pkgStr, depth+1)
				if err != nil {
					errMu.Lock()
					errs = errors.Join(errs, err)
					errMu.Unlock()
				}
			})
		}

		wg.Wait()
		return errs
	}

	var errs error
	for name, version := range allDependencies {
		pkgStr := fmt.Sprintf("%s@%s", name, version)
		err := handleNpmPkg(ctx, pkgStr, depth+1)
		if err != nil {
			errs = errors.Join(errs, err)
		}
	}

	return errs
}

// fetchOutputTarball downloads and saves the tarball for a given package version.
func fetchOutputTarball(pkg NpmPackage, outputDir string) (string, error) {
	if pkg.Dist.Tarball == "" {
		return "", fmt.Errorf("tarball URL is empty for: %s@%s", pkg.Name, pkg.Version)
	}

	cleanName := strings.TrimPrefix(pkg.Name, "@")
	cleanName = strings.ReplaceAll(cleanName, "/", "-")
	filename := fmt.Sprintf("%s-%s.tgz", cleanName, pkg.Version)
	destPath := filepath.Join(outputDir, filename)

	_, err := os.Stat(destPath)
	if err == nil {
		return destPath, nil // file exists
	}

	err = os.MkdirAll(outputDir, 0755)
	if err != nil {
		return "", fmt.Errorf("failed to create output directory: %w", err)
	}

	u, err := url.Parse(pkg.Dist.Tarball)
	if err != nil {
		return "", fmt.Errorf("invalid URL %s: %w", pkg.Dist.Tarball, err)
	}
	secureURL := u.String()

	resp, err := http.Get(secureURL)
	if err != nil {
		return "", fmt.Errorf("failed to download tarball from %s: %w", secureURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("received non-200 status code (%s) for %s", resp.Status, secureURL)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return "", fmt.Errorf("failed to create file %s: %w", destPath, err)
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to write download data: %w", err)
	}

	return destPath, nil
}

type FullRegistryResponse struct {
	Name     string                     `json:"name"`
	DistTags map[string]string          `json:"dist-tags"`
	Versions map[string]json.RawMessage `json:"versions"`
}

// fetchPackageMetadata queries the npm registry and resolves version constraints.
func fetchPackageMetadata(ctx context.Context, pkgStr string) ([]byte, error) {
	name, versionConstraint := parsePkgSpec(pkgStr)

	escapedName := name
	after, ok := strings.CutPrefix(name, "@")
	if ok {
		escapedName = "@" + url.PathEscape(after)
	}

	apiURL := fmt.Sprintf("https://registry.npmjs.org/%s", escapedName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for %s: %w", pkgStr, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch %s metadata from registry: %w", pkgStr, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("npm package '%s' not found (status: %s)", name, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body for %s: %w", pkgStr, err)
	}

	var reg FullRegistryResponse
	err = json.Unmarshal(body, &reg)
	if err != nil {
		return nil, fmt.Errorf("failed to parse registry json for %s: %w", pkgStr, err)
	}

	var targetVersion string
	if exactVer, ok := reg.DistTags[versionConstraint]; ok {
		targetVersion = exactVer
	} else if _, ok := reg.Versions[versionConstraint]; ok {
		targetVersion = versionConstraint
	} else {
		constraint, parseErr := semver.NewConstraint(versionConstraint)
		if parseErr != nil {
			if latest, ok := reg.DistTags["latest"]; ok {
				targetVersion = latest
			} else {
				return nil, fmt.Errorf("invalid version constraint '%s' for %s: %w", versionConstraint, name, parseErr)
			}
		} else {
			var maxVer *semver.Version
			for vStr := range reg.Versions {
				v, vErr := semver.NewVersion(vStr)
				if vErr != nil {
					continue
				}
				if constraint.Check(v) {
					if maxVer == nil || v.GreaterThan(maxVer) {
						maxVer = v
					}
				}
			}
			if maxVer == nil {
				return nil, fmt.Errorf("no version matching constraint '%s' found for %s", versionConstraint, name)
			}
			targetVersion = maxVer.Original()
		}
	}

	rawVersionMeta, ok := reg.Versions[targetVersion]
	if !ok {
		return nil, fmt.Errorf("version %s not found in registry versions for package %s", targetVersion, name)
	}

	return rawVersionMeta, nil
}

// parsePkgSpec extracts the package name and version range from a spec string.
func parsePkgSpec(pkgStr string) (string, string) {
	if strings.HasPrefix(pkgStr, "@") {
		idx := strings.Index(pkgStr[1:], "@")
		if idx == -1 {
			return pkgStr, "latest"
		}
		actualIdx := idx + 1
		return pkgStr[:actualIdx], pkgStr[actualIdx+1:]
	}

	parts := strings.SplitN(pkgStr, "@", 2)
	if len(parts) == 1 || parts[1] == "" {
		return parts[0], "latest"
	}
	return parts[0], parts[1]
}

// sanitizeTarball reads a .tgz file, strips unwanted keys from package/package.json,
// and writes a new sanitized .tgz file to the destination path.
func sanitizeTarball(srcPath, destPath string) error {
	file, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer file.Close()

	gzr, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)

	outFile, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	gzw := gzip.NewWriter(outFile)
	defer gzw.Close()

	tw := tar.NewWriter(gzw)
	defer tw.Close()

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// fix for some older .tgz npm package practises
		if header.Typeflag == tar.TypeDir {
			header.Mode |= 0755 // add permissions without removing what is already set
		}

		cleanName := path.Clean(header.Name)
		parts := strings.Split(cleanName, "/")

		// A root package.json will have either:
		// 1. len == 1: "package.json" (flat archive)
		// 2. len == 2: "package/package.json" or "lodash-4.17.21/package.json" (standard top-level folder)
		isRootPackageJSON := false
		if (len(parts) == 1 && parts[0] == "package.json") || (len(parts) == 2 && parts[1] == "package.json") {
			isRootPackageJSON = true
		}

		if isRootPackageJSON {
			data, err := io.ReadAll(tr)
			if err != nil {
				return err
			}

			var pkg map[string]any
			if err := json.Unmarshal(data, &pkg); err != nil {
				return err
			}

			//delete(pkg, "bugs")
			//delete(pkg, "homepage")
			delete(pkg, "scripts")
			delete(pkg, "funding")
			delete(pkg, "repository")
			delete(pkg, "publishConfig")

			newData, err := json.MarshalIndent(pkg, "", "  ")
			if err != nil {
				return err
			}

			header.Size = int64(len(newData))
			if err := tw.WriteHeader(header); err != nil {
				return err
			}
			if _, err := tw.Write(newData); err != nil {
				return err
			}
		} else {
			if err := tw.WriteHeader(header); err != nil {
				return err
			}
			if _, err := io.Copy(tw, tr); err != nil {
				return err
			}
		}
	}

	return nil
}

// formatBytes formats bytes into human size
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// useColor determine whether to use color
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
