package npm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// TODO: not done !!

// PackageJSON represents the minimum fields we need to read out of the archive
type PackageJSON struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

func PublishPackage(ctx context.Context, tarballPath, registryURL, authToken string) error {
	// 1. Read the tarball from disk
	tarballBytes, err := os.ReadFile(tarballPath)
	if err != nil {
		return fmt.Errorf("failed to read tarball: %w", err)
	}

	// 2. Extract package.json from inside the .tgz file to grab name & version
	pkgJSON, err := extractPackageJSON(tarballBytes)
	if err != nil {
		return fmt.Errorf("failed to extract package.json from tgz: %w", err)
	}

	// 3. Base64 encode the tarball binary data
	base64Data := base64.StdEncoding.EncodeToString(tarballBytes)
	filename := filepath.Base(tarballPath)

	// 4. Construct the standard npm registry publication document body
	payload := map[string]any{
		"_id":         pkgJSON.Name,
		"name":        pkgJSON.Name,
		"description": pkgJSON.Description,
		"versions": map[string]any{
			pkgJSON.Version: pkgJSON, // embeds the full package.json object for this version
		},
		"dist-tags": map[string]string{
			"latest": pkgJSON.Version,
		},
		"_attachments": map[string]any{
			filename: map[string]any{
				"content_type": "application/octet-stream",
				"data":         base64Data,
				"length":       len(tarballBytes),
			},
		},
	}

	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal json payload: %w", err)
	}

	// 5. Send HTTP PUT request to the custom registry endpoint
	// Endpoint format: {registryURL}/{package-name}
	targetURL := fmt.Sprintf("%s/%s", registryURL, pkgJSON.Name)
	req, err := http.NewRequest("PUT", targetURL, bytes.NewBuffer(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create http request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", authToken))

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("registry rejected publish (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	fmt.Printf("Successfully published %s@%s via HTTP!\n", pkgJSON.Name, pkgJSON.Version)
	return nil
}

// Helper function to read package/package.json out of a gzipped tar archive
func extractPackageJSON(tgzBytes []byte) (*PackageJSON, error) {
	gzReader, err := gzip.NewReader(bytes.NewReader(tgzBytes))
	if err != nil {
		return nil, err
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		// npm tarballs store contents inside a root folder named "package/"
		if header.Name == "package/package.json" {
			data, err := io.ReadAll(tarReader)
			if err != nil {
				return nil, err
			}

			var pkg PackageJSON
			if err := json.Unmarshal(data, &pkg); err != nil {
				return nil, err
			}
			return &pkg, nil
		}
	}

	return nil, fmt.Errorf("package.json not found inside tarball archive")
}
