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
)

var (
	fileMu   sync.Mutex
	seenMu   sync.Mutex
	printMu  sync.Mutex
	seenPkgs = make(map[string]struct{})

	errFlagHelp = errors.New("flag help")

	flagOutputDir  *string
	flagConcurrent *bool
	flagIncludeDev *bool
	flagUseIndent  *bool
	flagOffline    *bool
)

type NpmPackage struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Dist                 NpmDist           `json:"dist"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

type NpmDist struct {
	Tarball   string `json:"tarball"`
	Integrity string `json:"integrity"`
	Shasum    string `json:"shasum"`
}

// main is the entry point of the application.
func main() {
	now := time.Now()
	ctx := context.Background()
	err := run(ctx)
	if err != nil {
		if errors.Is(err, errFlagHelp) {
			return // flag help output
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stdout, "\nFinished in %.2fs\n", time.Since(now).Seconds())
}

// run parses command-line flags and dispatches execution to file or package handlers.
func run(ctx context.Context) error {
	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)

	flagOutputDir = fs.String("output", "./npm-bundle", "Target directory for downloaded tarballs (set to empty to omit building output)")
	flagConcurrent = fs.Bool("concurrent", false, "Enable concurrent downloads and dependency traversal")
	flagIncludeDev = fs.Bool("dev", false, "Include devDependencies. Warning: this will take a long time to bundle!")
	flagUseIndent = fs.Bool("indent", false, "Print with indent to see package hierarchy")
	flagOffline = fs.Bool("offline", false, "Sanitize package.json (remove scripts, repository, etc.) for airgapped environments")

	err := fs.Parse(os.Args[1:])
	if err != nil {
		return errFlagHelp
	}

	args := fs.Args()
	if len(args) == 0 {
		return fmt.Errorf("missing file or package argument (e.g. file.txt or pkgname@version)")
	}

	input := args[0]
	if strings.Contains(input, "@") {
		return handleNpmPkg(ctx, input, 0)
	}

	fi, err := os.Stat(input)
	if err != nil {
		return handleNpmPkg(ctx, input, 0)
	}

	if fi.IsDir() {
		return fmt.Errorf("argument can't be a directory")
	}

	return handleFile(ctx, input)
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
		return fmt.Errorf("npm package '%s' not found: %w", pkgStr, err)
	}

	var pkg NpmPackage
	err = json.Unmarshal(outByte, &pkg)
	if err != nil {
		return fmt.Errorf("failed to parse package '%s' metadata: %w", pkgStr, err)
	}

	return handleDeps(ctx, pkg, depth)
}

// handleDeps outputs the tarball and recursively processes all package dependencies.
func handleDeps(ctx context.Context, pkg NpmPackage, depth int) error {
	if pkg.Name == "" || pkg.Version == "" {
		return nil
	}

	printMu.Lock()
	indent := strings.Repeat("  ", depth)
	if depth == 0 {
		fmt.Printf("processing: %s@%s\n", pkg.Name, pkg.Version)
	} else {
		if *flagUseIndent {
			fmt.Printf("%s%s@%s\n", indent, pkg.Name, pkg.Version)
		} else {
			fmt.Printf("  %s@%s\n", pkg.Name, pkg.Version)
		}
	}
	printMu.Unlock()

	if *flagOutputDir != "" {
		fileMu.Lock()

		outPath, err := fetchOutputTarball(pkg, *flagOutputDir)
		if err != nil {
			fileMu.Unlock()
			return fmt.Errorf("failed to generate tarball for %s: %w", pkg.Name+"@"+pkg.Version, err)
		}

		if *flagOffline {

			tmpPath := outPath + ".tmp"
			err = sanitizeTarball(outPath, tmpPath)
			if err != nil {
				os.Remove(tmpPath)
				fileMu.Unlock()
				return fmt.Errorf("failed to sanitize tarball '%s' for offline use: %w", outPath, err)
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

	if *flagIncludeDev {
		// WARN: this will take a long time to bundle
		maps.Copy(allDependencies, pkg.DevDependencies)
	}

	if *flagConcurrent {
		var wg sync.WaitGroup
		var errMu sync.Mutex
		var combinedErr error

		for name, version := range allDependencies {
			pkgStr := fmt.Sprintf("%s@%s", name, version)
			wg.Go(func() {
				// NOTE: add rate limit?
				err := handleNpmPkg(ctx, pkgStr, depth+1)
				if err != nil {
					errMu.Lock()
					combinedErr = errors.Join(combinedErr, err)
					errMu.Unlock()
				}
			})
		}

		wg.Wait()
		return combinedErr
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

	err := os.MkdirAll(outputDir, 0755)
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

	cleanName := strings.TrimPrefix(pkg.Name, "@")
	cleanName = strings.ReplaceAll(cleanName, "/", "-")
	filename := fmt.Sprintf("%s-%s.tgz", cleanName, pkg.Version)
	destPath := filepath.Join(outputDir, filename)

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
		return nil, fmt.Errorf("failed to fetch metadata from registry: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("npm package '%s' not found (status: %s)", name, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var reg FullRegistryResponse
	err = json.Unmarshal(body, &reg)
	if err != nil {
		return nil, fmt.Errorf("failed to parse registry json: %w", err)
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
