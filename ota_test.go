package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func otaTestBinary(label string) []byte {
	switch runtime.GOOS {
	case "darwin", "ios":
		return append([]byte{0xcf, 0xfa, 0xed, 0xfe}, []byte(label)...)
	case "windows":
		return []byte("MZ00" + label)
	default:
		return []byte("\x7fELF" + label)
	}
}

type otaFixture struct {
	runner    otaRunner
	manifest  otaManifest
	raw       []byte
	signature []byte
	body      []byte
	requests  atomic.Int32
	downloads atomic.Int32
	server    *httptest.Server
	old       []byte
}

func otaNewFixture(t *testing.T) *otaFixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &otaFixture{body: otaTestBinary("new-binary"), old: otaTestBinary("old-binary")}
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		switch r.URL.Path {
		case "/manifest":
			_, _ = w.Write(f.raw)
		case "/manifest.sig":
			_, _ = w.Write(f.signature)
		case "/binary":
			f.downloads.Add(1)
			_, _ = w.Write(f.body)
		case "/redirect":
			http.Redirect(w, r, "http://example.invalid/binary", http.StatusFound)
		case "/tls-redirect":
			http.Redirect(w, r, f.server.URL+"/binary", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	h := sha256.Sum256(f.body)
	f.manifest = otaManifest{Version: "v0.1.1", Assets: []otaAsset{{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: f.server.URL + "/binary", SHA256: hex.EncodeToString(h[:]), Size: int64(len(f.body))}}}
	f.sign(t, priv)
	p := filepath.Join(t.TempDir(), "app")
	if err := os.WriteFile(p, f.old, 0751); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0751); err != nil {
		t.Fatal(err)
	}
	f.runner = otaRunner{client: f.server.Client(), manifestURL: f.server.URL + "/manifest", publicKey: pub, executable: p, version: "v0.1.0", out: &bytes.Buffer{}}
	return f
}
func (f *otaFixture) sign(t *testing.T, key ed25519.PrivateKey) {
	t.Helper()
	raw, err := json.Marshal(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	// Preserve intentional leading/trailing JSON whitespace in signed bytes.
	f.raw = append(append([]byte("\n  "), raw...), '\n')
	f.signature = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(key, f.raw)) + "\n")
}
func (f *otaFixture) resign(t *testing.T) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.runner.publicKey = pub
	f.sign(t, key)
}
func otaAssertFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("file %s changed: got %q want %q", path, got, want)
	}
}
func otaAssertClean(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ota-") || strings.HasSuffix(e.Name(), ".update.lock") {
			t.Errorf("leftover OTA file %s", e.Name())
		}
	}
}

func TestOTACheckGoodSignatureNoMutation(t *testing.T) {
	f := otaNewFixture(t)
	if err := f.runner.run([]string{"check"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.runner.out.(*bytes.Buffer).String(), "v0.1.1 available") {
		t.Fatal("missing version availability")
	}
	otaAssertFile(t, f.runner.executable, f.old)
	if f.downloads.Load() != 0 {
		t.Fatal("check downloaded executable")
	}
	if _, err := os.Stat(f.runner.executable + ".previous"); !os.IsNotExist(err) {
		t.Fatalf("check created backup: %v", err)
	}
	otaAssertClean(t, f.runner.executable)
}
func TestOTAUpdateAndRollback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix atomic replacement")
	}
	f := otaNewFixture(t)
	if err := f.runner.run([]string{"update"}); err != nil {
		t.Fatal(err)
	}
	otaAssertFile(t, f.runner.executable, f.body)
	otaAssertFile(t, f.runner.executable+".previous", f.old)
	for _, p := range []string{f.runner.executable, f.runner.executable + ".previous"} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0751 {
			t.Fatalf("mode %o", st.Mode().Perm())
		}
	}
	if !strings.Contains(f.runner.out.(*bytes.Buffer).String(), "Restart needed") {
		t.Fatal("missing restart instruction")
	}
	if err := os.Chmod(f.runner.executable, 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.run([]string{"rollback"}); err != nil {
		t.Fatal(err)
	}
	otaAssertFile(t, f.runner.executable, f.old)
	st, err := os.Stat(f.runner.executable)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0751 {
		t.Fatalf("rollback mode %o", st.Mode().Perm())
	}
	// Copy-style rollback retains the backup, permitting another recovery attempt.
	otaAssertFile(t, f.runner.executable+".previous", f.old)
	otaAssertClean(t, f.runner.executable)
}
func TestOTABadSignatureBeforeParse(t *testing.T) {
	for _, invalidJSON := range []bool{false, true} {
		t.Run(fmt.Sprint(invalidJSON), func(t *testing.T) {
			f := otaNewFixture(t)
			if invalidJSON {
				f.raw = []byte("not JSON")
			} else {
				f.raw = append(f.raw, ' ')
			}
			err := f.runner.run([]string{"update"})
			if err == nil || !strings.Contains(err.Error(), "signature") {
				t.Fatalf("expected signature error before parsing, got %v", err)
			}
			otaAssertFile(t, f.runner.executable, f.old)
			if f.downloads.Load() != 0 {
				t.Fatal("unverified manifest triggered download")
			}
		})
	}
}
func TestOTAChecksumMismatchPreservesBinary(t *testing.T) {
	f := otaNewFixture(t)
	f.body[len(f.body)-1] ^= 1
	err := f.runner.run([]string{"update"})
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("expected checksum error, got %v", err)
	}
	otaAssertFile(t, f.runner.executable, f.old)
	if _, err := os.Stat(f.runner.executable + ".previous"); !os.IsNotExist(err) {
		t.Fatal("failed download replaced backup")
	}
	otaAssertClean(t, f.runner.executable)
}
func TestOTAPlatformMismatch(t *testing.T) {
	t.Run("manifest", func(t *testing.T) {
		f := otaNewFixture(t)
		f.manifest.Assets[0].OS = "other-os"
		f.resign(t)
		if err := f.runner.run([]string{"update"}); err == nil || !strings.Contains(err.Error(), "no OTA asset") {
			t.Fatalf("got %v", err)
		}
		otaAssertFile(t, f.runner.executable, f.old)
	})
	t.Run("magic", func(t *testing.T) {
		f := otaNewFixture(t)
		f.body = []byte("#!/bin/sh\necho not an executable\n")
		h := sha256.Sum256(f.body)
		f.manifest.Assets[0].Size = int64(len(f.body))
		f.manifest.Assets[0].SHA256 = hex.EncodeToString(h[:])
		f.resign(t)
		if err := f.runner.run([]string{"update"}); err == nil || !strings.Contains(err.Error(), "format") {
			t.Fatalf("got %v", err)
		}
		otaAssertFile(t, f.runner.executable, f.old)
		otaAssertClean(t, f.runner.executable)
	})
}
func TestOTAOutdatedAndEqualDoNotDownload(t *testing.T) {
	for _, version := range []string{"v0.0.9", "v0.1.0"} {
		t.Run(version, func(t *testing.T) {
			f := otaNewFixture(t)
			f.manifest.Version = version
			f.resign(t)
			if err := f.runner.run([]string{"update"}); err != nil {
				t.Fatal(err)
			}
			if f.downloads.Load() != 0 {
				t.Fatal("downloaded non-newer version")
			}
			otaAssertFile(t, f.runner.executable, f.old)
		})
	}
}
func TestOTADownloadBound(t *testing.T) {
	t.Run("manifest-limit", func(t *testing.T) {
		f := otaNewFixture(t)
		f.manifest.Assets[0].Size = otaMaxDownload + 1
		f.resign(t)
		if err := f.runner.run([]string{"update"}); err == nil {
			t.Fatal("accepted asset above 256 MiB")
		}
		if f.downloads.Load() != 0 {
			t.Fatal("downloaded oversized asset")
		}
		otaAssertFile(t, f.runner.executable, f.old)
	})
	t.Run("declared-size", func(t *testing.T) {
		f := otaNewFixture(t)
		f.manifest.Assets[0].Size = 4
		f.resign(t)
		if err := f.runner.run([]string{"update"}); err == nil {
			t.Fatal("accepted response above declared size")
		}
		otaAssertFile(t, f.runner.executable, f.old)
		otaAssertClean(t, f.runner.executable)
	})
	t.Run("chunked-size", func(t *testing.T) {
		f := otaNewFixture(t)
		f.runner.client = &http.Client{Transport: otaRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/binary" {
				return f.server.Client().Transport.RoundTrip(req)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 100))), ContentLength: -1, Header: make(http.Header)}, nil
		})}
		if err := f.runner.run([]string{"update"}); err == nil || !strings.Contains(err.Error(), "size mismatch") {
			t.Fatalf("got %v", err)
		}
		otaAssertFile(t, f.runner.executable, f.old)
		otaAssertClean(t, f.runner.executable)
	})
}

type otaRoundTripFunc func(*http.Request) (*http.Response, error)

func (f otaRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOTAHTTPSEnforcementAndRedirects(t *testing.T) {
	for _, raw := range []string{"http://example.com/x", "file:///tmp/x", "https://user:secret@example.com/x", "https://example.com/x#fragment", "/relative"} {
		if err := otaHTTPS(raw); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	t.Run("manifest-http", func(t *testing.T) {
		f := otaNewFixture(t)
		f.runner.manifestURL = "http://example.invalid/manifest"
		if err := f.runner.run([]string{"check"}); err == nil {
			t.Fatal("accepted HTTP manifest")
		}
		if f.requests.Load() != 0 {
			t.Fatal("sent request")
		}
	})
	t.Run("asset-http", func(t *testing.T) {
		f := otaNewFixture(t)
		f.manifest.Assets[0].URL = "http://example.invalid/binary"
		f.resign(t)
		if err := f.runner.run([]string{"check"}); err == nil {
			t.Fatal("accepted HTTP asset")
		}
	})
	for _, path := range []string{"/redirect", "/tls-redirect"} {
		t.Run(path, func(t *testing.T) {
			f := otaNewFixture(t)
			f.manifest.Assets[0].URL = f.server.URL + path
			f.resign(t)
			if err := f.runner.run([]string{"update"}); err == nil || !strings.Contains(err.Error(), "302") {
				t.Fatalf("followed redirect: %v", err)
			}
			if f.downloads.Load() != 0 {
				t.Fatal("redirect was followed")
			}
			otaAssertFile(t, f.runner.executable, f.old)
		})
	}
}
func TestOTASemver(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{{"v1.0.0", "v0.99.99", true}, {"v0.10.0", "v0.9.99", true}, {"v0.1.1", "v0.1.0", true}, {"v0.1.0", "v0.1.0", false}, {"v0.1.0", "v0.2.0", false}, {"v999999999999999999999999.0.0", "v2.0.0", true}} {
		got, err := otaVersionNewer(tc.a, tc.b)
		if err != nil || got != tc.want {
			t.Fatalf("%s > %s got %v, %v", tc.a, tc.b, got, err)
		}
	}
	for _, v := range []string{"1.2.3", "v01.2.3", "v1.2", "v1.2.3-rc.1", "v1.2.3+build", "v-1.2.3", "v1.2.3\n"} {
		if _, err := otaVersionNewer(v, "v0.1.0"); err == nil {
			t.Errorf("accepted %q", v)
		}
	}
}
func TestOTADuplicateAsset(t *testing.T) {
	f := otaNewFixture(t)
	f.manifest.Assets = append(f.manifest.Assets, f.manifest.Assets[0])
	f.resign(t)
	if err := f.runner.run([]string{"check"}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("got %v", err)
	}
}
func TestOTALockAndSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlink and mode semantics")
	}
	f := otaNewFixture(t)
	unlock, err := otaLock(f.runner.executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.runner.run([]string{"update"}); err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("got %v", err)
	}
	otaAssertFile(t, f.runner.executable, f.old)
	unlock()
	link := filepath.Join(filepath.Dir(f.runner.executable), "alias")
	if err := os.Symlink(f.runner.executable, link); err != nil {
		t.Fatal(err)
	}
	f.runner.executable = link
	if err := f.runner.run([]string{"update"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("update replaced symlink rather than target")
	}
	otaAssertFile(t, link, f.body)
}
func TestOTAKeygenAndSignRawBytes(t *testing.T) {
	dir := t.TempDir()
	pubPath := filepath.Join(dir, "public")
	privPath := filepath.Join(dir, "private")
	r := otaRunner{}
	if err := r.run([]string{"keygen", pubPath, privPath}); err != nil {
		t.Fatal(err)
	}
	pubText, err := os.ReadFile(pubPath)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(pubText)))
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{pubPath: 0644, privPath: 0600} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && st.Mode().Perm() != want {
			t.Fatalf("%s mode %o", p, st.Mode().Perm())
		}
	}
	if err := r.run([]string{"keygen", pubPath, privPath}); err == nil {
		t.Fatal("keygen overwrote keys")
	}
	otaAssertFile(t, pubPath, pubText)
	f := otaNewFixture(t)
	manifestPath := filepath.Join(dir, "manifest")
	sigPath := filepath.Join(dir, "signature")
	if err := os.WriteFile(manifestPath, f.raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.run([]string{"sign", manifestPath, privPath, sigPath}); err != nil {
		t.Fatal(err)
	}
	sigText, err := os.ReadFile(sigPath)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigText)))
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), f.raw, sig) {
		t.Fatal("signature did not cover original bytes")
	}
	if err := r.run([]string{"sign", manifestPath, privPath, sigPath}); err == nil {
		t.Fatal("sign overwrote signature")
	}
	if err := os.WriteFile(manifestPath, []byte(`{"version":"v1.0.0","assets":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.run([]string{"sign", manifestPath, privPath, filepath.Join(dir, "invalid.sig")}); err == nil {
		t.Fatal("signed invalid manifest")
	}
}
func TestOTAKeygenExistingPublicLeavesNoPrivate(t *testing.T) {
	dir := t.TempDir()
	pub := filepath.Join(dir, "public")
	priv := filepath.Join(dir, "private")
	if err := os.WriteFile(pub, []byte("existing"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := otaKeygen(pub, priv); err == nil {
		t.Fatal("overwrote existing public")
	}
	otaAssertFile(t, pub, []byte("existing"))
	if _, err := os.Stat(priv); !os.IsNotExist(err) {
		t.Fatalf("partial private key remains: %v", err)
	}
}
func TestOTAUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"check", "extra"}, {"update", "extra"}, {"rollback", "extra"}, {"keygen"}, {"sign"}} {
		if err := (otaRunner{}).run(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
}
