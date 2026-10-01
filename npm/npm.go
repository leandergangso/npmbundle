package npm

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
)

var (
	httpClient = &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
	}
)

type Package struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Dist                 PackageDist       `json:"dist"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

type PackageDist struct {
	Shasum       string `json:"shasum"`
	Tarball      string `json:"tarball"`
	Integrity    string `json:"integrity"`
	UnpackedSize int64  `json:"unpackedSize"`
	FileCount    int    `json:"fileCount"`
}

type RegistryResponse struct {
	Name     string                     `json:"name"`
	DistTags map[string]string          `json:"dist-tags"`
	Versions map[string]json.RawMessage `json:"versions"`
}

// FetchPackageMetadata fetches metadata for a given npm pkg
//
//	example: axios
//	example: axios@0.30.0
//	example: axios@latest
func FetchPackageMetadata(ctx context.Context, pkgSpec string) (*Package, error) {
	pkgName, pkgConstraint := SplitPackageSpec(pkgSpec)

	escapedName := pkgName
	after, ok := strings.CutPrefix(pkgName, "@")
	if ok {
		escapedName = "@" + url.PathEscape(after)
	}

	apiURL := fmt.Sprintf("https://registry.npmjs.org/%s", escapedName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for %s: %w", pkgName, err)
	}
	req.Header.Set("Accept", "application/vnd.npm.install-v1+json") // get limited metadata for reduced size and bandwidth

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch %s metadata from registry: %w", pkgName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("npm package '%s' not found (status: %s)", pkgName, resp.Status)
	}

	var reg RegistryResponse
	err = json.NewDecoder(resp.Body).Decode(&reg)
	if err != nil {
		return nil, fmt.Errorf("failed to decode registry json for %s: %w", pkgName, err)
	}

	if reg.Name == "" || len(reg.Versions) == 0 {
		return nil, fmt.Errorf("malformed package: %s", pkgSpec)
	}

	var targetVer string
	if exactVer, ok := reg.DistTags[pkgConstraint]; ok {
		targetVer = exactVer
	} else if _, ok := reg.Versions[pkgConstraint]; ok {
		targetVer = pkgConstraint
	} else {
		var versions []string
		for vStr := range reg.Versions {
			versions = append(versions, vStr)
		}
		maxVer, err := ResolveConstraint(pkgConstraint, versions)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve version for pkg %s: %w", pkgSpec, err)
		}
		targetVer = maxVer
	}

	rawVersionMeta, ok := reg.Versions[targetVer]
	if !ok {
		return nil, fmt.Errorf("version %s not found in registry versions for package %s", targetVer, pkgName)
	}

	var pkg = new(Package)
	err = json.Unmarshal(rawVersionMeta, pkg)
	if err != nil {
		return nil, fmt.Errorf("failed to decode registry json for %s: %w", pkgName, err)
	}

	return pkg, nil
}

// DownloadTarball downloads npm pkg tarball, places it in a temp location,
// sanitizes it if requested, and moves it to the output dir.
func DownloadTarball(ctx context.Context, pkg *Package, outputDir string, sanitize bool) (int64, error) {
	if pkg.Dist.Tarball == "" {
		return 0, fmt.Errorf("tarball URL is empty for: %s@%s", pkg.Name, pkg.Version)
	}

	var cleanName = pkg.Name
	if after, ok := strings.CutPrefix(cleanName, "@"); ok {
		cleanName = after
		cleanName = strings.ReplaceAll(cleanName, "/", "__")
	}
	filename := fmt.Sprintf("%s-%s.tgz", cleanName, pkg.Version)
	outputFile := filepath.Join(outputDir, filename)

	_, err := os.Stat(outputFile)
	if err == nil {
		return 0, nil // file exists
	}

	err = os.MkdirAll(outputDir, 0755)
	if err != nil {
		return 0, fmt.Errorf("failed to create output directory %s: %w", outputDir, err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", pkg.Dist.Tarball, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request for %s: %w", pkg.Dist.Tarball, err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to download tarball from %s: %w", pkg.Dist.Tarball, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("received non-200 status code (%s) for %s", resp.Status, pkg.Dist.Tarball)
	}

	file, err := os.Create(outputFile)
	if err != nil {
		return 0, fmt.Errorf("failed to create file %s: %w", filename, err)
	}

	var success bool
	defer func() {
		file.Close()
		if !success {
			_ = os.Remove(outputFile) // clean up partial files
		}
	}()

	if sanitize {
		err = sanitizeTarball(file, resp.Body)
		if err != nil {
			return 0, fmt.Errorf("failed to sanitize tarball: %w", err)
		}
	} else {
		_, err = io.Copy(file, resp.Body)
		if err != nil {
			return 0, fmt.Errorf("failed to write download data: %w", err)
		}
	}

	stat, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("failed to stat file %s: %w", filename, err)
	}

	success = true
	return stat.Size(), nil
}

// SanitizeTarball reads tarball body, strips unwanted fields from package.json, and writes to file
func sanitizeTarball(file *os.File, body io.ReadCloser) error {
	gzR, err := gzip.NewReader(body)
	if err != nil {
		return err
	}
	defer gzR.Close()
	tarR := tar.NewReader(gzR)

	gzW := gzip.NewWriter(file)
	defer gzW.Close()
	tarW := tar.NewWriter(gzW)
	defer tarW.Close()

	for {
		header, err := tarR.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}

		if header.Typeflag == tar.TypeDir {
			header.Mode |= 0755
		}

		cleanName := path.Clean(header.Name)
		parts := strings.Split(cleanName, "/")

		isRootPackageJSON := false
		if (len(parts) == 1 && parts[0] == "package.json") || (len(parts) == 2 && parts[1] == "package.json") {
			isRootPackageJSON = true
		}

		if isRootPackageJSON {
			data, err := io.ReadAll(tarR)
			if err != nil {
				return err
			}

			var pkg map[string]json.RawMessage
			err = json.Unmarshal(data, &pkg)
			if err != nil {
				return err
			}

			delete(pkg, "scripts")
			delete(pkg, "funding")
			delete(pkg, "repository")
			delete(pkg, "publishConfig")

			newData, err := json.MarshalIndent(pkg, "", "  ")
			if err != nil {
				return err
			}

			header.Size = int64(len(newData))
			err = tarW.WriteHeader(header)
			if err != nil {
				return err
			}

			_, err = tarW.Write(newData)
			if err != nil {
				return err
			}
		} else {
			err = tarW.WriteHeader(header)
			if err != nil {
				return err
			}
			_, err = io.Copy(tarW, tarR)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// ResolveSpec takes pkg name and constraint(version) and resolves it into its proper pkg spec
func ResolveSpec(name, constraint string) string {
	var spec string
	if after, ok := strings.CutPrefix(constraint, "npm:"); ok {
		spec = after
	} else {
		spec = fmt.Sprintf("%s@%s", name, constraint)
	}
	return spec
}

// SplitPackageSpec take a pkg string and splits it into its name and version
func SplitPackageSpec(spec string) (string, string) {
	if strings.HasPrefix(spec, "@") {
		idx := strings.Index(spec[1:], "@")
		if idx == -1 {
			return spec, "latest"
		}
		actualIdx := idx + 1
		return spec[:actualIdx], spec[actualIdx+1:]
	}

	parts := strings.SplitN(spec, "@", 2)
	if len(parts) == 1 || parts[1] == "" {
		return parts[0], "latest"
	}

	return parts[0], parts[1]
}

// ResolveConstraint takes the tag constraint and a list of all available tags and returns the highest allowed tag
func ResolveConstraint(tagConstraint string, availableVersions []string) (string, error) {
	var targetV string

	c, err := semver.NewConstraint(tagConstraint)
	if err != nil {
		return "", err
	}

	var maxV *semver.Version
	for _, tag := range availableVersions {
		v, err := semver.NewVersion(tag)
		if err != nil {
			continue
		}
		if c.Check(v) {
			if maxV == nil || v.GreaterThan(maxV) {
				maxV = v
			}
		}
	}
	if maxV == nil {
		return "", fmt.Errorf("no version matching constraint '%s'", tagConstraint)
	}
	targetV = maxV.Original()

	return targetV, nil
}

// FormatSize takes a byte size and returns human formatted string
func FormatSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}
