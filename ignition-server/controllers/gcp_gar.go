package controllers

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/elf"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/vincent-petithory/dataurl"
)

const garCredentialHelperConfigPath = "/etc/hypershift/gar-credential-helper.json"

// configureGCPGARWorker adds the helper after MCS rendering, keeping the binary
// outside API objects. The small MachineConfig marker is part of the existing
// NodePool config hash, including the immutable helper image and architecture.
func configureGCPGARWorker(payload []byte, loadHelper func(string) ([]byte, error)) ([]byte, error) {
	var root, storage map[string]json.RawMessage
	var files []json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, err
	}
	if len(root["storage"]) == 0 {
		return payload, nil
	}
	if err := json.Unmarshal(root["storage"], &storage); err != nil {
		return nil, err
	}
	if len(storage["files"]) == 0 {
		return payload, nil
	}
	if err := json.Unmarshal(storage["files"], &files); err != nil {
		return nil, err
	}
	var helperConfig []byte
	paths := map[string]bool{}
	for _, raw := range files {
		var file map[string]json.RawMessage
		if err := json.Unmarshal(raw, &file); err != nil {
			return nil, err
		}
		var path string
		if err := json.Unmarshal(file["path"], &path); err != nil {
			return nil, err
		}
		paths[path] = true
		if path == garCredentialHelperConfigPath {
			var err error
			helperConfig, err = garInlineFileContent(file)
			if err != nil {
				return nil, fmt.Errorf("decode GAR helper configuration: %w", err)
			}
		}
	}
	if helperConfig == nil {
		return payload, nil
	}
	var config struct {
		Image        string   `json:"image"`
		Architecture string   `json:"architecture"`
		Registries   []string `json:"registries"`
	}
	if err := json.Unmarshal(helperConfig, &config); err != nil {
		return nil, fmt.Errorf("decode GAR helper configuration: %w", err)
	}
	if config.Architecture != "amd64" {
		return nil, fmt.Errorf("GAR helper POC requires an amd64 NodePool")
	}
	if !regexp.MustCompile(`@sha256:[a-f0-9]{64}$`).MatchString(config.Image) {
		return nil, fmt.Errorf("GAR helper image must be pinned by SHA-256 digest")
	}
	if len(config.Registries) == 0 {
		return nil, fmt.Errorf("GAR helper requires at least one registry hostname")
	}
	for _, host := range config.Registries {
		if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*-docker\.pkg\.dev$`).MatchString(host) {
			return nil, fmt.Errorf("invalid GAR registry hostname %q", host)
		}
	}
	for _, path := range []string{
		"/usr/local/bin/docker-credential-gcr", "/var/usrlocal/bin/docker-credential-gcr",
		"/etc/containers/gar-auth.json", "/etc/ostree/auth.json",
		"/etc/crio/crio.conf.d/99-gar-credentials.conf",
		"/etc/containers/gcr-helper.json",
		"/etc/systemd/system/rpm-ostreed.service.d/99-gar-credentials.conf",
	} {
		if paths[path] {
			return nil, fmt.Errorf("GAR helper would overwrite an existing Ignition file: %s", path)
		}
	}
	helper, err := loadHelper(config.Image)
	if err != nil {
		return nil, fmt.Errorf("extract Google credential helper: %w", err)
	}
	binary, err := elf.NewFile(bytes.NewReader(helper))
	if err != nil {
		return nil, fmt.Errorf("Google credential helper is not an ELF executable: %w", err)
	}
	if binary.Machine != elf.EM_X86_64 || binary.Class != elf.ELFCLASS64 {
		return nil, fmt.Errorf("Google credential helper is not an amd64 executable")
	}
	compressed := &bytes.Buffer{}
	w := gzip.NewWriter(compressed)
	if _, err := w.Write(helper); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	file, err := json.Marshal(map[string]any{
		"path": "/usr/local/bin/docker-credential-gcr", "mode": 0755,
		"contents": map[string]any{
			"source":       dataurl.New(compressed.Bytes(), "application/octet-stream").String(),
			"compression":  "gzip",
			"verification": map[string]string{"hash": fmt.Sprintf("sha256-%x", sha256.Sum256(helper))},
		},
	})
	if err != nil {
		return nil, err
	}
	files = append(files, file)
	files, err = configureGARNativeAuth(files, config.Registries)
	if err != nil {
		return nil, err
	}
	storage["files"], err = json.Marshal(files)
	if err != nil {
		return nil, err
	}
	root["storage"], err = json.Marshal(storage)
	if err != nil {
		return nil, err
	}
	return json.Marshal(root)
}

// configureGARNativeAuth changes final, rendered Ignition storage.files.
// Hosts are registry names, e.g. us-central1-docker.pkg.dev, without paths.
// docker-credential-gcr must already be installed and on each consumer's PATH.
func configureGARNativeAuth(files []json.RawMessage, hosts []string) ([]json.RawMessage, error) {
	if len(hosts) == 0 {
		return files, nil
	}
	helperHosts := map[string]string{}
	for _, host := range hosts {
		helperHosts[host] = "gcr"
	}
	bootstrapFound := false
	var mergedConfig []byte
	for i, raw := range files {
		var file map[string]json.RawMessage
		if err := json.Unmarshal(raw, &file); err != nil {
			return nil, err
		}
		var path string
		if err := json.Unmarshal(file["path"], &path); err != nil {
			return nil, err
		}
		if path != "/var/lib/kubelet/config.json" {
			continue
		}
		content, err := garInlineFileContent(file)
		if err != nil {
			return nil, fmt.Errorf("decode kubelet pull secret: %w", err)
		}
		var config map[string]json.RawMessage
		if err := json.Unmarshal(content, &config); err != nil || config == nil {
			return nil, fmt.Errorf("kubelet pull secret is not a JSON object: %v", err)
		}
		var auths map[string]json.RawMessage
		if rawAuths := config["auths"]; len(rawAuths) > 0 {
			if err := json.Unmarshal(rawAuths, &auths); err != nil {
				return nil, err
			}
		}
		for registry := range auths {
			host := strings.TrimPrefix(strings.TrimPrefix(registry, "https://"), "http://")
			host = strings.SplitN(host, "/", 2)[0]
			if _, gar := helperHosts[host]; gar {
				delete(auths, registry)
			}
		}
		if auths == nil {
			auths = map[string]json.RawMessage{}
		}
		config["auths"], _ = json.Marshal(auths)
		var helpers map[string]string
		if rawHelpers := config["credHelpers"]; len(rawHelpers) > 0 {
			if err := json.Unmarshal(rawHelpers, &helpers); err != nil {
				return nil, err
			}
		}
		if helpers == nil {
			helpers = map[string]string{}
		}
		for host := range helperHosts {
			helpers[host] = "gcr"
		}
		config["credHelpers"], _ = json.Marshal(helpers)
		content, err = json.Marshal(config)
		if err != nil {
			return nil, err
		}
		files[i] = garInlineFile(file, content, 0600)
		mergedConfig = content
		bootstrapFound = true
	}
	if !bootstrapFound {
		return nil, fmt.Errorf("rendered Ignition has no /var/lib/kubelet/config.json")
	}
	// Preserve source credentials for non-GAR private images. These dedicated
	// auth snapshots are not updated by MCO's subsequent pull-secret rotation.
	crioConfig := []byte("[crio.image]\nglobal_auth_file = \"/etc/containers/gar-auth.json\"\npause_image_auth_file = \"/etc/containers/gar-auth.json\"\n")
	// rpm-ostree may run its image reader without HOME under a dynamic UID.
	// Explicit official-helper paths avoid depending on a user's config directory.
	ostreeEnvironment := []byte("[Service]\nEnvironment=PATH=/usr/local/bin:/usr/local/sbin:/usr/bin:/usr/sbin\nEnvironment=DOCKER_CREDENTIAL_GCR_CONFIG=/etc/containers/gcr-helper.json\nEnvironment=DOCKER_CREDENTIAL_GCR_STORE=/etc/containers/gcr-unused-store.json\n")
	for _, addition := range []struct {
		path    string
		content []byte
		mode    int
	}{
		{"/etc/containers/gar-auth.json", mergedConfig, 0600},
		{"/etc/ostree/auth.json", mergedConfig, 0600},
		{"/etc/crio/crio.conf.d/99-gar-credentials.conf", crioConfig, 0644},
		{"/etc/containers/gcr-helper.json", []byte(`{"TokenSources":["env"]}`), 0644},
		{"/etc/systemd/system/rpm-ostreed.service.d/99-gar-credentials.conf", ostreeEnvironment, 0644},
	} {
		file := map[string]json.RawMessage{}
		file["path"], _ = json.Marshal(addition.path)
		files = append(files, garInlineFile(file, addition.content, addition.mode))
	}
	return files, nil
}

func garInlineFileContent(file map[string]json.RawMessage) ([]byte, error) {
	var contents struct {
		Source      string `json:"source"`
		Compression string `json:"compression"`
	}
	if err := json.Unmarshal(file["contents"], &contents); err != nil {
		return nil, err
	}
	url, err := dataurl.DecodeString(contents.Source)
	if err != nil {
		return nil, err
	}
	if contents.Compression == "" {
		return url.Data, nil
	}
	if contents.Compression != "gzip" {
		return nil, fmt.Errorf("unsupported compression %q", contents.Compression)
	}
	r, err := gzip.NewReader(bytes.NewReader(url.Data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func garInlineFile(file map[string]json.RawMessage, content []byte, mode int) json.RawMessage {
	// Existing verification/compression belong to the old contents. Re-encode as
	// an uncompressed inline source while retaining all other file-level fields.
	contents := map[string]json.RawMessage{}
	if original := file["contents"]; len(original) > 0 {
		_ = json.Unmarshal(original, &contents)
	}
	delete(contents, "compression")
	delete(contents, "verification")
	contents["source"], _ = json.Marshal(dataurl.New(content, "application/octet-stream").String())
	file["contents"], _ = json.Marshal(contents)
	file["mode"], _ = json.Marshal(mode)
	file["overwrite"] = json.RawMessage("true")
	delete(file, "append")
	raw, _ := json.Marshal(file)
	return raw
}
