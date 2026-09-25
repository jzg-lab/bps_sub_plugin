// packager 构建各平台运行时、生成 manifest.json、可选签名，并打成 .s2plugin 包。
//
//	go run ./tools/packager                       # 未签名开发包（宿主需 plugins.allow_unsigned: true）
//	go run ./tools/packager -key secrets/pub.key  # 签名发布包
//	go run ./tools/packager keygen -out secrets/  # 生成 Ed25519 发布密钥
package main

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jzg-lab/bps_sub_plugin/internal/buildinfo"
	"github.com/jzg-lab/bps_sub_plugin/internal/plugin"
)

const (
	schemaVersion = 1
	mainPackage   = "./cmd/bps-plugin"
)

// source 对应 manifest.source.json：手写部分，其余字段由打包器生成。
type source struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Author       string          `json:"author"`
	Requires     json.RawMessage `json:"requires"`
	Capabilities []capability    `json:"capabilities"`
	Targets      []string        `json:"targets"`
	UI           uiEntry         `json:"ui"`
}

type capability struct {
	ID          string `json:"id"`
	Platform    string `json:"platform"`
	AccountType string `json:"account_type"`
}

type uiEntry struct {
	Entrypoint string `json:"entrypoint"`
}

type runtimeEntry struct {
	Path string `json:"path"`
}

// manifest 字段顺序与宿主 PluginManifest 一致。
type manifest struct {
	SchemaVersion int                     `json:"schema_version"`
	ID            string                  `json:"id"`
	Name          string                  `json:"name"`
	Version       string                  `json:"version"`
	Description   string                  `json:"description,omitempty"`
	Author        string                  `json:"author,omitempty"`
	Requires      json.RawMessage         `json:"requires"`
	Capabilities  []capability            `json:"capabilities"`
	Runtimes      map[string]runtimeEntry `json:"runtimes"`
	UI            uiEntry                 `json:"ui"`
	Files         map[string]string       `json:"files"`
}

type signature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "keygen" {
		if err := keygen(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "keygen:", err)
			os.Exit(1)
		}
		return
	}
	if err := pack(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "packager:", err)
		os.Exit(1)
	}
}

func pack(args []string) error {
	flags := flag.NewFlagSet("packager", flag.ContinueOnError)
	sourcePath := flags.String("source", "manifest.source.json", "手写清单")
	uiDir := flags.String("ui", "ui", "UI 静态文件目录")
	outDir := flags.String("out", "dist", "输出目录")
	targetList := flags.String("targets", "", "逗号分隔的目标平台，覆盖清单里的 targets，例如 linux-amd64")
	keyPath := flags.String("key", "", "Ed25519 私钥文件（keygen 生成）；为空则输出未签名包")
	keyID := flags.String("key-id", "jzg-lab-bps-v1", "签名密钥 ID，需与宿主 plugins.trusted_publishers 的键一致")
	if err := flags.Parse(args); err != nil {
		return err
	}

	var src source
	if err := readJSONStrict(*sourcePath, &src); err != nil {
		return err
	}
	if err := checkSource(src); err != nil {
		return err
	}
	targets := src.Targets
	if *targetList != "" {
		targets = strings.Split(*targetList, ",")
	}
	if len(targets) == 0 {
		return errors.New("至少需要一个目标平台")
	}

	staging, err := os.MkdirTemp("", "bps-plugin-pack-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	files := map[string]string{}
	runtimes := map[string]runtimeEntry{}
	for _, target := range targets {
		target = strings.TrimSpace(target)
		goos, goarch, ok := strings.Cut(target, "-")
		if !ok || goos == "" || goarch == "" {
			return fmt.Errorf("目标平台格式应为 <goos>-<goarch>: %q", target)
		}
		binary := "plugin"
		if goos == "windows" {
			binary += ".exe"
		}
		relative := "runtimes/" + target + "/" + binary
		if err := build(goos, goarch, filepath.Join(staging, filepath.FromSlash(relative))); err != nil {
			return fmt.Errorf("构建 %s: %w", target, err)
		}
		runtimes[target] = runtimeEntry{Path: relative}
	}
	if err := copyTree(*uiDir, filepath.Join(staging, "ui")); err != nil {
		return fmt.Errorf("复制 UI: %w", err)
	}
	err = filepath.WalkDir(staging, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, _ := filepath.Rel(staging, path)
		digest, err := sha256File(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = digest
		return nil
	})
	if err != nil {
		return err
	}
	if _, ok := files[src.UI.Entrypoint]; !ok {
		return fmt.Errorf("UI 入口 %s 不存在", src.UI.Entrypoint)
	}

	m := manifest{
		SchemaVersion: schemaVersion,
		ID:            buildinfo.PluginID,
		Name:          src.Name,
		Version:       buildinfo.Version,
		Description:   src.Description,
		Author:        src.Author,
		Requires:      src.Requires,
		Capabilities:  src.Capabilities,
		Runtimes:      runtimes,
		UI:            src.UI,
		Files:         files,
	}
	manifestRaw, err := marshalIndent(m)
	if err != nil {
		return err
	}

	entries := map[string][]byte{"manifest.json": manifestRaw}
	signed := false
	if *keyPath != "" {
		privateKey, err := readPrivateKey(*keyPath)
		if err != nil {
			return err
		}
		sig, err := marshalIndent(signature{
			Algorithm: "ed25519",
			KeyID:     *keyID,
			Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifestRaw)),
		})
		if err != nil {
			return err
		}
		entries["signature.json"] = sig
		signed = true
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	suffix := ""
	if !signed {
		suffix = "-unsigned"
	}
	output := filepath.Join(*outDir, fmt.Sprintf("%s-%s%s.s2plugin", buildinfo.PluginID, buildinfo.Version, suffix))
	if err := writeZip(output, staging, files, entries); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*outDir, "manifest.json"), manifestRaw, 0o644); err != nil {
		return err
	}
	fmt.Printf("已生成 %s（%d 个文件，签名：%v）\n", output, len(files), signed)
	return nil
}

func checkSource(src source) error {
	if strings.TrimSpace(src.Name) == "" {
		return errors.New("name 不能为空")
	}
	if len(src.Requires) == 0 {
		return errors.New("requires 不能为空")
	}
	if len(src.Capabilities) != 1 || src.Capabilities[0].ID != plugin.Capability {
		return fmt.Errorf("capabilities 必须且只能是 %s", plugin.Capability)
	}
	if !strings.HasPrefix(src.UI.Entrypoint, "ui/") {
		return errors.New("ui.entrypoint 必须位于 ui/ 目录")
	}
	return nil
}

func build(goos, goarch, output string) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w -buildid=", "-o", output, mainPackage)
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(from, path)
		target := filepath.Join(to, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("UI 目录只能包含普通文件: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func writeZip(output, staging string, files map[string]string, extra map[string][]byte) error {
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	modified := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	add := func(name string, data []byte, mode os.FileMode) error {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: modified}
		header.SetMode(mode)
		writer, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}
		_, err = writer.Write(data)
		return err
	}
	for _, name := range sortedKeys(extra) {
		if err := add(name, extra[name], 0o644); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(files) {
		data, err := os.ReadFile(filepath.Join(staging, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(name, "runtimes/") {
			mode = 0o755
		}
		if err := add(name, data, mode); err != nil {
			return err
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	return os.WriteFile(output, buffer.Bytes(), 0o644)
}

func keygen(args []string) error {
	flags := flag.NewFlagSet("keygen", flag.ContinueOnError)
	outDir := flags.String("out", "secrets", "密钥输出目录（已在 .gitignore 中）")
	if err := flags.Parse(args); err != nil {
		return err
	}
	privatePath := filepath.Join(*outDir, "publisher.key")
	if _, err := os.Stat(privatePath); err == nil {
		return fmt.Errorf("%s 已存在，拒绝覆盖", privatePath)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(privatePath, []byte(base64.StdEncoding.EncodeToString(privateKey.Seed())+"\n"), 0o600); err != nil {
		return err
	}
	encodedPublic := base64.StdEncoding.EncodeToString(publicKey)
	if err := os.WriteFile(filepath.Join(*outDir, "publisher.pub"), []byte(encodedPublic+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("私钥：%s（不要提交、不要上传到服务器）\n公钥：%s\n", privatePath, encodedPublic)
	return nil
}

func readPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("私钥文件格式无效，应为 base64 编码的 32 字节种子")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// marshalIndent 输出带换行的缩进 JSON，不转义 <、>、&（版本范围里有 >= 和 <）。
func marshalIndent(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func readJSONStrict(path string, target any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("解析 %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("%s 只能包含一个 JSON 对象", path)
	}
	return nil
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
