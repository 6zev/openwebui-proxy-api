package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const otaMaxDownload int64 = 256 << 20
const otaMaxManifest int64 = 1 << 20

// otaRunner holds test seams only. Production configuration deliberately has no
// executable-path, private-key, transport, or TLS-verification environment knob.
type otaRunner struct {
	client      *http.Client
	manifestURL string
	publicKey   ed25519.PublicKey
	executable  string
	version     string
	out         io.Writer
}
type otaManifest struct {
	Version string     `json:"version"`
	Assets  []otaAsset `json:"assets"`
}
type otaAsset struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func runOTA(args []string) error {
	r := otaRunner{version: Version, out: os.Stdout}
	if len(args) == 1 && (args[0] == "check" || args[0] == "update") {
		r.manifestURL = os.Getenv("OTA_MANIFEST_URL")
		key, err := base64.StdEncoding.DecodeString(os.Getenv("OTA_PUBLIC_KEY"))
		if err != nil || len(key) != ed25519.PublicKeySize {
			return errors.New("OTA_PUBLIC_KEY must be a base64 Ed25519 public key")
		}
		r.publicKey = ed25519.PublicKey(key)
	}
	return r.run(args)
}

func (r otaRunner) run(args []string) error {
	if r.out == nil {
		r.out = io.Discard
	}
	if len(args) == 0 {
		return errors.New("usage: ota check|update|rollback|keygen|sign")
	}
	switch args[0] {
	case "keygen":
		if len(args) != 3 {
			return errors.New("usage: ota keygen PUBLIC_FILE PRIVATE_FILE")
		}
		return otaKeygen(args[1], args[2])
	case "sign":
		if len(args) != 4 {
			return errors.New("usage: ota sign MANIFEST PRIVATE_KEY SIGNATURE_FILE")
		}
		return otaSign(args[1], args[2], args[3])
	case "rollback":
		if len(args) != 1 {
			return errors.New("usage: ota rollback")
		}
		return r.rollback()
	case "check", "update":
		if len(args) != 1 {
			return errors.New("usage: ota check|update")
		}
		m, a, err := r.fetchManifest()
		if err != nil {
			return err
		}
		newer, err := otaVersionNewer(m.Version, r.version)
		if err != nil {
			return err
		}
		if !newer {
			_, err = fmt.Fprintf(r.out, "No update available (current %s, manifest %s).\n", r.version, m.Version)
			return err
		}
		if args[0] == "check" {
			_, err = fmt.Fprintf(r.out, "Version %s available (current %s).\n", m.Version, r.version)
			return err
		}
		return r.update(a, m.Version)
	default:
		return errors.New("usage: ota check|update|rollback|keygen|sign")
	}
}

var otaRelease = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func otaVersionNewer(candidate, current string) (bool, error) {
	a, b := otaRelease.FindStringSubmatch(candidate), otaRelease.FindStringSubmatch(current)
	if a == nil || b == nil {
		return false, errors.New("versions must be release semver vX.Y.Z without leading zeros")
	}
	// Decimal-string comparison avoids integer overflow, including signed manifests
	// containing version components larger than the host's word size.
	for i := 1; i <= 3; i++ {
		if len(a[i]) != len(b[i]) {
			return len(a[i]) > len(b[i]), nil
		}
		if a[i] != b[i] {
			return a[i] > b[i], nil
		}
	}
	return false, nil
}

func otaHTTPS(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("OTA URLs must use HTTPS without credentials or fragments")
	}
	return nil
}

func otaParseManifest(raw []byte) (otaManifest, error) {
	var m otaManifest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, fmt.Errorf("invalid OTA manifest: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return m, errors.New("OTA manifest must contain exactly one JSON object")
	}
	if _, err := otaVersionNewer(m.Version, "v0.0.0"); err != nil {
		return m, err
	}
	if len(m.Assets) == 0 {
		return m, errors.New("OTA manifest has no assets")
	}
	seen := make(map[string]bool)
	for _, a := range m.Assets {
		if a.OS == "" || a.Arch == "" {
			return m, errors.New("OTA asset requires os and arch")
		}
		id := a.OS + "/" + a.Arch
		if seen[id] {
			return m, fmt.Errorf("duplicate OTA asset for %s", id)
		}
		seen[id] = true
		if err := otaHTTPS(a.URL); err != nil {
			return m, err
		}
		h, err := hex.DecodeString(a.SHA256)
		if err != nil || len(h) != sha256.Size {
			return m, errors.New("OTA asset requires a 64-character SHA-256 digest")
		}
		if a.Size <= 0 || a.Size > otaMaxDownload {
			return m, errors.New("OTA asset size must be between 1 byte and 256 MiB")
		}
	}
	return m, nil
}

// Even injected clients have redirects disabled; the injection changes only the
// trusted CA/transport for tests, never the URL policy.
func (r otaRunner) httpClient() *http.Client {
	c := http.Client{Timeout: 5 * time.Minute}
	if r.client != nil {
		c = *r.client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if c.Timeout == 0 {
		c.Timeout = 5 * time.Minute
	}
	return &c
}

func (r otaRunner) get(raw string) (*http.Response, error) {
	if err := otaHTTPS(raw); err != nil {
		return nil, err
	}
	resp, err := r.httpClient().Get(raw)
	if err != nil {
		return nil, fmt.Errorf("OTA request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("OTA HTTP status %d (redirects are disabled)", resp.StatusCode)
	}
	return resp, nil
}

func (r otaRunner) fetchSmall(raw string, limit int64) ([]byte, error) {
	resp, err := r.get(raw)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("OTA response exceeds size limit")
	}
	return b, nil
}

func (r otaRunner) fetchManifest() (otaManifest, otaAsset, error) {
	var m otaManifest
	var asset otaAsset
	if len(r.publicKey) != ed25519.PublicKeySize {
		return m, asset, errors.New("OTA public key must contain 32 bytes")
	}
	raw, err := r.fetchSmall(r.manifestURL, otaMaxManifest)
	if err != nil {
		return m, asset, err
	}
	detached, err := r.fetchSmall(r.manifestURL+".sig", 1024)
	if err != nil {
		return m, asset, err
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(detached)))
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(r.publicKey, raw, sig) {
		return m, asset, errors.New("OTA manifest signature verification failed")
	}
	// Parsing is intentionally after verification of the exact downloaded bytes.
	m, err = otaParseManifest(raw)
	if err != nil {
		return m, asset, err
	}
	for _, a := range m.Assets {
		if a.OS == runtime.GOOS && a.Arch == runtime.GOARCH {
			return m, a, nil
		}
	}
	return m, asset, fmt.Errorf("no OTA asset for %s/%s", runtime.GOOS, runtime.GOARCH)
}

func (r otaRunner) executablePath() (string, error) {
	p := r.executable
	var err error
	if p == "" {
		p, err = os.Executable()
		if err != nil {
			return "", err
		}
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

func otaLock(binary string) (func(), error) {
	path := binary + ".update.lock"
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("acquire OTA lock: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return func() { _ = os.Remove(path) }, nil
}

func otaExecutableMagic(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var h [4]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return errors.New("OTA asset has no valid executable header")
	}
	ok := false
	switch runtime.GOOS {
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly", "solaris", "illumos", "android":
		ok = bytes.Equal(h[:], []byte{0x7f, 'E', 'L', 'F'})
	case "darwin", "ios":
		switch h {
		case [4]byte{0xfe, 0xed, 0xfa, 0xce}, [4]byte{0xce, 0xfa, 0xed, 0xfe}, [4]byte{0xfe, 0xed, 0xfa, 0xcf}, [4]byte{0xcf, 0xfa, 0xed, 0xfe}, [4]byte{0xca, 0xfe, 0xba, 0xbe}, [4]byte{0xbe, 0xba, 0xfe, 0xca}, [4]byte{0xca, 0xfe, 0xba, 0xbf}, [4]byte{0xbf, 0xba, 0xfe, 0xca}:
			ok = true
		}
	case "windows":
		ok = h[0] == 'M' && h[1] == 'Z'
	}
	if !ok {
		return fmt.Errorf("OTA asset executable format does not match %s", runtime.GOOS)
	}
	return nil
}

// otaStageCopy never exposes an incomplete backup/replacement. The returned
// same-directory file is already synced, closed and assigned its final mode.
func otaStageCopy(src, dir string, mode os.FileMode) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("OTA source must be a regular file")
	}
	out, err := os.CreateTemp(dir, ".ota-copy-*")
	if err != nil {
		return "", err
	}
	name := out.Name()
	success := false
	defer func() {
		_ = out.Close()
		if !success {
			_ = os.Remove(name)
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return "", err
	}
	if err = out.Chmod(mode.Perm()); err != nil {
		return "", err
	}
	if err = out.Sync(); err != nil {
		return "", err
	}
	if err = out.Close(); err != nil {
		return "", err
	}
	success = true
	return name, nil
}

func otaSyncDir(dir string) {
	if f, err := os.Open(dir); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
}

func (r otaRunner) update(a otaAsset, version string) error {
	if runtime.GOOS == "windows" {
		return errors.New("in-place OTA replacement is supported on Unix only")
	}
	if a.Size <= 0 || a.Size > otaMaxDownload {
		return errors.New("OTA asset exceeds download size limit")
	}
	binary, err := r.executablePath()
	if err != nil {
		return err
	}
	unlock, err := otaLock(binary)
	if err != nil {
		return err
	}
	defer unlock()
	info, err := os.Stat(binary)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("OTA executable must be a regular file")
	}
	dir := filepath.Dir(binary)
	f, err := os.CreateTemp(dir, ".ota-download-*") // 0600 until verification finishes.
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = f.Close(); _ = os.Remove(tmp) }()
	resp, err := r.get(a.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.ContentLength > a.Size {
		return errors.New("OTA download exceeds declared size")
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(resp.Body, a.Size+1))
	if err != nil {
		return err
	}
	if n != a.Size {
		return errors.New("OTA download size mismatch")
	}
	expected, err := hex.DecodeString(a.SHA256)
	if err != nil || !bytes.Equal(hash.Sum(nil), expected) {
		return errors.New("OTA download SHA-256 mismatch")
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := otaExecutableMagic(tmp); err != nil {
		return err
	}
	if err := f.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	backup, err := otaStageCopy(binary, dir, info.Mode())
	if err != nil {
		return err
	}
	defer os.Remove(backup)
	if err := os.Rename(backup, binary+".previous"); err != nil {
		return err
	}
	otaSyncDir(dir)
	if err := os.Rename(tmp, binary); err != nil {
		return err
	}
	otaSyncDir(dir)
	// Installation is committed independently of the output destination.
	_, _ = fmt.Fprintf(r.out, "Updated to %s. Restart needed; current process was not restarted.\n", version)
	return nil
}

func (r otaRunner) rollback() error {
	if runtime.GOOS == "windows" {
		return errors.New("in-place OTA rollback is supported on Unix only")
	}
	binary, err := r.executablePath()
	if err != nil {
		return err
	}
	unlock, err := otaLock(binary)
	if err != nil {
		return err
	}
	defer unlock()
	previous := binary + ".previous"
	info, err := os.Lstat(previous)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("OTA backup must be a regular file")
	}
	if err := otaExecutableMagic(previous); err != nil {
		return err
	}
	dir := filepath.Dir(binary)
	staged, err := otaStageCopy(previous, dir, info.Mode())
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	if err := os.Rename(staged, binary); err != nil {
		return err
	}
	otaSyncDir(dir)
	_, _ = fmt.Fprintln(r.out, "Rollback complete. Restart needed; current process was not restarted.")
	return nil
}

func otaWriteNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		_ = f.Close()
		if !success {
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	success = true
	return nil
}

func otaKeygen(publicFile, privateFile string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := otaWriteNew(privateFile, []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0600); err != nil {
		return err
	}
	if err := otaWriteNew(publicFile, []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0644); err != nil {
		_ = os.Remove(privateFile)
		return err
	}
	return nil
}

func otaReadLocal(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("OTA local file exceeds size limit")
	}
	return b, nil
}

func otaSign(manifestFile, privateFile, signatureFile string) error {
	raw, err := otaReadLocal(manifestFile, otaMaxManifest)
	if err != nil {
		return err
	}
	if _, err := otaParseManifest(raw); err != nil {
		return err
	}
	encoded, err := otaReadLocal(privateFile, 1024)
	if err != nil {
		return err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return errors.New("private key must be a base64 64-byte Ed25519 private key")
	}
	// Reject malformed expanded keys rather than producing unverifiable signatures.
	derived := ed25519.NewKeyFromSeed(key[:ed25519.SeedSize])
	if !bytes.Equal(key, derived) {
		return errors.New("Ed25519 private key is inconsistent")
	}
	sig := ed25519.Sign(ed25519.PrivateKey(key), raw)
	return otaWriteNew(signatureFile, []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0644)
}
