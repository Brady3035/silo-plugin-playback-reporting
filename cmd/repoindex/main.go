// Command repoindex writes the Silo plugin repository index (repository.json)
// for one release: the plugin's manifest plus a download URL and SHA-256
// checksum for each platform binary. Silo reads the index from a plugin
// repository URL and installs or auto-updates the plugin from it.
//
//	go run ./cmd/repoindex -version 0.3.0 -repo owner/name -dist release -out repository.json
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// Platforms the release builds, as os/arch.
var Platforms = []string{"linux/amd64", "linux/arm64", "darwin/arm64"}

// Index mirrors Silo's plugin repository index (internal/plugins.RepositoryIndex).
type Index struct {
	Plugins []Package `json:"plugins"`
}

type Package struct {
	Manifest     *pluginv1.PluginManifest `json:"manifest"`
	RepoURL      string                   `json:"repo_url"`
	ChecksumsURL string                   `json:"checksums_url"`
	Binaries     map[string]Binary        `json:"binaries"`
}

type Binary struct {
	URL      string `json:"url"`
	Checksum string `json:"checksum"`
}

func main() {
	version := flag.String("version", "", "release version, without the leading v")
	repo := flag.String("repo", "", "GitHub repository, owner/name")
	dist := flag.String("dist", "release", "directory holding plugin-<os>-<arch> binaries")
	manifestPath := flag.String("manifest", "manifest.json", "plugin manifest")
	out := flag.String("out", "repository.json", "index to write")
	flag.Parse()
	if *version == "" || *repo == "" {
		fail("-version and -repo are required")
	}
	index, err := Build(*manifestPath, *version, *repo, *dist)
	if err != nil {
		fail(err.Error())
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		fail(err.Error())
	}
	if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
		fail(err.Error())
	}
}

// Build reads the manifest and the release binaries and returns the index.
func Build(manifestPath, version, repo, dist string) (*Index, error) {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	m := &pluginv1.PluginManifest{}
	if err := protojson.Unmarshal(raw, m); err != nil {
		return nil, fmt.Errorf("read %s: %w", manifestPath, err)
	}
	version = strings.TrimPrefix(version, "v")
	m.Version = version
	// The checksum belongs to each binary; the catalog lists it per platform.
	m.Checksum = ""
	m.SupportedPlatforms = nil
	repoURL := "https://github.com/" + repo
	release := repoURL + "/releases/download/v" + version + "/"
	pkg := Package{Manifest: m, RepoURL: repoURL, ChecksumsURL: release + "checksums.txt", Binaries: map[string]Binary{}}
	for _, p := range Platforms {
		goos, arch, _ := strings.Cut(p, "/")
		name := "plugin-" + goos + "-" + arch
		data, err := os.ReadFile(filepath.Join(dist, name))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		pkg.Binaries[p] = Binary{URL: release + name, Checksum: hex.EncodeToString(sum[:])}
		m.SupportedPlatforms = append(m.SupportedPlatforms, &pluginv1.SupportedPlatform{Os: goos, Arch: arch})
	}
	if m.Presentation != nil {
		m.Presentation.HomepageUrl = repoURL
		m.Presentation.SourceUrl = repoURL
	}
	return &Index{Plugins: []Package{pkg}}, nil
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "repoindex:", msg)
	os.Exit(1)
}
