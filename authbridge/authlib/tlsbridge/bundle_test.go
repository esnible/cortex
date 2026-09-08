package tlsbridge

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCA and fakeRoots stand in for real PEM: EnsureTrustBundle concatenates
// rather than parses, so the bytes only need the BEGIN marker findSystemRoots
// looks for.
const (
	fakeCA    = "-----BEGIN CERTIFICATE-----\nCORTEXCA\n-----END CERTIFICATE-----\n"
	fakeRoots = "-----BEGIN CERTIFICATE-----\nPUBLICROOT\n-----END CERTIFICATE-----\n"
)

// withSystemRoots points the package at a temp root store for the duration of a
// test, so a test's outcome does not depend on what the host happens to ship.
func withSystemRoots(t *testing.T, contents string) {
	t.Helper()
	orig := systemRootFiles
	t.Cleanup(func() { systemRootFiles = orig })
	if contents == "" {
		systemRootFiles = []string{filepath.Join(t.TempDir(), "absent.pem")}
		return
	}
	p := filepath.Join(t.TempDir(), "roots.pem")
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	systemRootFiles = []string{p}
}

func caDirWith(t *testing.T, ca string) string {
	t.Helper()
	dir := t.TempDir()
	if ca != "" {
		if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte(ca), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestEnsureTrustBundle_ContainsBothTrustSets is the whole point: a tool pointed
// at this file must trust the bridge AND the public internet. A bundle holding
// only one of them is the bug.
func TestEnsureTrustBundle_ContainsBothTrustSets(t *testing.T) {
	withSystemRoots(t, fakeRoots)
	dir := caDirWith(t, fakeCA)

	path, err := EnsureTrustBundle(dir)
	if err != nil {
		t.Fatalf("EnsureTrustBundle: %v", err)
	}
	if got := filepath.Base(path); got != TrustBundleName {
		t.Errorf("bundle name = %q, want %q", got, TrustBundleName)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("CORTEXCA")) {
		t.Error("bundle is missing the bridge CA")
	}
	if !bytes.Contains(data, []byte("PUBLICROOT")) {
		t.Error("bundle is missing the platform roots — every direct TLS call would fail")
	}
	if n := strings.Count(string(data), "BEGIN CERTIFICATE"); n != 2 {
		t.Errorf("certificate count = %d, want 2", n)
	}
	// CA first, so a verifier finds it without walking the public roots.
	if bytes.Index(data, []byte("CORTEXCA")) > bytes.Index(data, []byte("PUBLICROOT")) {
		t.Error("bridge CA should precede the platform roots")
	}
}

// TestEnsureTrustBundle_NoSystemRootsWritesNothing: the failure mode that matters.
// Falling back to a CA-only bundle would hand every tool a trust store containing
// one private CA — silently distrusting the public internet, which is far worse
// than not being able to verify the bridge.
func TestEnsureTrustBundle_NoSystemRootsWritesNothing(t *testing.T) {
	withSystemRoots(t, "")
	dir := caDirWith(t, fakeCA)

	_, err := EnsureTrustBundle(dir)
	if !errors.Is(err, ErrNoSystemRoots) {
		t.Fatalf("error = %v, want ErrNoSystemRoots", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, TrustBundleName)); serr == nil {
		t.Fatal("a CA-only bundle was written; it must not exist at all")
	}
}

// TestEnsureTrustBundle_EmptyRootFileIsSkipped: some minimal images ship an empty
// placeholder at a well-known path. Accepting it would produce a CA-only bundle
// by a different route than the check above.
func TestEnsureTrustBundle_EmptyRootFileIsSkipped(t *testing.T) {
	orig := systemRootFiles
	t.Cleanup(func() { systemRootFiles = orig })
	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, []byte("# no certs here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(t.TempDir(), "real.pem")
	if err := os.WriteFile(real, []byte(fakeRoots), 0o644); err != nil {
		t.Fatal(err)
	}
	systemRootFiles = []string{empty, real}

	dir := caDirWith(t, fakeCA)
	path, err := EnsureTrustBundle(dir)
	if err != nil {
		t.Fatalf("EnsureTrustBundle: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte("PUBLICROOT")) {
		t.Error("skipped past the empty placeholder to the real store, but the roots are absent")
	}
}

// TestEnsureTrustBundle_Idempotent: called on every boot, so an unchanged bundle
// must not be rewritten — that would churn the mtime and disturb a reader holding
// it open.
func TestEnsureTrustBundle_Idempotent(t *testing.T) {
	withSystemRoots(t, fakeRoots)
	dir := caDirWith(t, fakeCA)

	path, err := EnsureTrustBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = EnsureTrustBundle(dir); err != nil {
		t.Fatal(err)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Error("unchanged bundle was rewritten")
	}
}

// TestEnsureTrustBundle_RewritesAfterCARotation: the flip side of idempotency. A
// stale bundle after the CA is regenerated would leave every tool unable to verify
// the bridge, with a file that looks present and correct.
func TestEnsureTrustBundle_RewritesAfterCARotation(t *testing.T) {
	withSystemRoots(t, fakeRoots)
	dir := caDirWith(t, fakeCA)

	path, err := EnsureTrustBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	rotated := "-----BEGIN CERTIFICATE-----\nROTATEDCA\n-----END CERTIFICATE-----\n"
	if werr := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte(rotated), 0o644); werr != nil {
		t.Fatal(werr)
	}
	if _, err = EnsureTrustBundle(dir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte("ROTATEDCA")) {
		t.Error("bundle still holds the old CA after rotation")
	}
	if bytes.Contains(data, []byte("CORTEXCA")) {
		t.Error("bundle kept the superseded CA")
	}
}

// TestEnsureTrustBundle_MissingCAFails: without ca.crt there is nothing to graft
// in, and writing the platform roots alone would produce a file that verifies
// everything EXCEPT the bridge — passing silently while parsing nothing.
func TestEnsureTrustBundle_MissingCAFails(t *testing.T) {
	withSystemRoots(t, fakeRoots)
	dir := caDirWith(t, "")

	if _, err := EnsureTrustBundle(dir); err == nil {
		t.Fatal("missing ca.crt should be an error")
	}
	if _, serr := os.Stat(filepath.Join(dir, TrustBundleName)); serr == nil {
		t.Error("a roots-only bundle was written")
	}
	if _, err := EnsureTrustBundle(""); err == nil {
		t.Error("empty ca_dir should be an error")
	}
}

// TestEnsureTrustBundle_SplicesPEMSafely: a CA file whose last line lacks a
// newline would otherwise join its END line to the next BEGIN, and every
// certificate after the splice silently fails to parse.
func TestEnsureTrustBundle_SplicesPEMSafely(t *testing.T) {
	withSystemRoots(t, fakeRoots)
	dir := caDirWith(t, strings.TrimSuffix(fakeCA, "\n")) // no trailing newline

	path, err := EnsureTrustBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte("-----END CERTIFICATE-------")) {
		t.Fatalf("PEM boundaries were spliced:\n%s", data)
	}
	if n := strings.Count(string(data), "BEGIN CERTIFICATE"); n != 2 {
		t.Errorf("certificate count = %d, want 2", n)
	}
}
