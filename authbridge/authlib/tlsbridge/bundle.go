package tlsbridge

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// TrustBundleName is the file EnsureTrustBundle writes inside a CA dir. It holds
// the bridge CA followed by the platform's trusted roots.
//
// It exists because ca.crt on its own is only usable by tools whose CA setting
// is ADDITIVE. Node's NODE_EXTRA_CA_CERTS is — the name says so — but almost
// every other tool REPLACES its trust store with what you point it at:
//
//	SSL_CERT_FILE      Go (gh, abctl, any Go CLI)
//	GIT_SSL_CAINFO     git
//	REQUESTS_CA_BUNDLE Python requests
//	CURL_CA_BUNDLE     curl
//
// Pointing those at ca.crt makes the process trust the bridge CA and NOTHING
// else, so every direct (unproxied) TLS connection fails. Go's own loader is
// explicit about it — crypto/x509 root_unix.go does `files = []string{f}` when
// SSL_CERT_FILE is set, discarding the defaults. The failure is platform-split
// and therefore easy to ship by accident: macOS uses root_darwin.go and the
// platform verifier, so a bare ca.crt appears to work there while breaking
// every Linux machine and CI runner.
const TrustBundleName = "bundle.crt"

// systemRootFiles are the locations that ship a concatenated PEM of the
// platform's trusted roots. Mirrors the list crypto/x509, OpenSSL, curl and git
// probe, so the bundle we assemble is the same trust set the tool would have
// used had we not overridden it.
//
// macOS has no such file for its keychain, but Apple ships LibreSSL's copy at
// /etc/ssl/cert.pem, which is the same list — and the tools that need this
// bundle (git, curl, Python) are the ones that read files rather than the
// keychain anyway.
var systemRootFiles = []string{
	"/etc/ssl/certs/ca-certificates.crt",                // Debian, Ubuntu, Gentoo, Alpine
	"/etc/pki/tls/certs/ca-bundle.crt",                  // Fedora, RHEL 6
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", // RHEL 7+, CentOS
	"/etc/ssl/ca-bundle.pem",                            // openSUSE
	"/etc/pki/tls/cacert.pem",                           // OpenELEC
	"/etc/ssl/cert.pem",                                 // macOS (LibreSSL), Alpine, OpenBSD
	"/usr/local/etc/ssl/cert.pem",                       // FreeBSD
}

// ErrNoSystemRoots means no platform root bundle could be located, so no trust
// bundle was written.
//
// Deliberately an error rather than a fallback to "the bridge CA alone": that
// file is the dangerous artifact this whole mechanism exists to avoid, and
// writing it would hand every tool a trust store containing one private CA. A
// missing bundle degrades to "tools keep their own trust and cannot verify the
// bridge", which is a visible, recoverable failure. The inverse — silently
// distrusting the public internet — is neither.
var ErrNoSystemRoots = errors.New("tlsbridge: no system root bundle found")

// EnsureTrustBundle writes <caDir>/bundle.crt as the bridge CA (<caDir>/ca.crt)
// followed by the platform's trusted roots, and returns its path.
//
// Idempotent: a bundle whose content already matches is left alone, so calling
// this on every boot neither churns the file nor disturbs a process that has it
// open. It re-reads both inputs, so a rotated CA or an updated root store is
// picked up — but only AT A CALL, and the only caller is the proxy at startup.
// For a service documented to run for weeks, that makes this a snapshot taken at
// boot, not a view of the store.
//
// The direction that matters is root REMOVAL: once the OS distrusts a root,
// every tool pointed at bundle.crt keeps trusting it until the proxy restarts,
// and nothing surfaces that. Addition is the benign half — a root added after
// boot is simply absent, and absence fails closed. Re-assembling on an interval
// would close the gap, and the content comparison above already makes that free
// of churn; it is not done yet because nothing has needed it.
//
// Callers must treat failure as non-fatal. The bridge itself works without a
// bundle — only the clients' ability to verify it is affected — so a boot that
// cannot assemble one should warn and continue, not exit.
func EnsureTrustBundle(caDir string) (string, error) {
	if caDir == "" {
		return "", errors.New("tlsbridge: empty ca_dir")
	}
	caPath := filepath.Join(caDir, "ca.crt")
	caPEM, err := os.ReadFile(caPath) //nolint:gosec // operator-supplied ca_dir
	if err != nil {
		return "", fmt.Errorf("tlsbridge: read bridge CA %s: %w", caPath, err)
	}
	rootsPath, rootsPEM, err := findSystemRoots()
	if err != nil {
		return "", err
	}

	// CA first: a verifier that stops at the first match spends no time walking
	// the public roots to find ours, and a human running `head` on the file sees
	// which CA was grafted in.
	var buf bytes.Buffer
	buf.Write(ensureTrailingNewline(caPEM))
	buf.WriteString("# --- platform roots from " + rootsPath + " ---\n")
	buf.Write(ensureTrailingNewline(rootsPEM))

	bundlePath := filepath.Join(caDir, TrustBundleName)
	if existing, rerr := os.ReadFile(bundlePath); rerr == nil && bytes.Equal(existing, buf.Bytes()) {
		return bundlePath, nil
	}
	// 0644 like ca.crt: this is public trust material, and every tool reading it
	// runs as the user or as another service account.
	if werr := atomicWriteFile(bundlePath, buf.Bytes(), 0o644); werr != nil {
		return "", fmt.Errorf("tlsbridge: write trust bundle %s: %w", bundlePath, werr)
	}
	return bundlePath, nil
}

// findSystemRoots returns the first readable entry in systemRootFiles along with
// its contents. A file that exists but holds no certificate is skipped rather
// than accepted: some minimal images ship an empty placeholder, and treating
// that as success would produce a CA-only bundle by a different route.
func findSystemRoots() (string, []byte, error) {
	for _, p := range systemRootFiles {
		data, err := os.ReadFile(p) //nolint:gosec // fixed list of well-known paths
		if err != nil {
			continue
		}
		if !bytes.Contains(data, []byte("-----BEGIN CERTIFICATE-----")) {
			continue
		}
		return p, data, nil
	}
	return "", nil, ErrNoSystemRoots
}

// ensureTrailingNewline guards the concatenation boundary: a PEM file whose last
// line lacks a newline would otherwise splice its END line onto the next BEGIN,
// and every certificate after that point silently fails to parse.
func ensureTrailingNewline(b []byte) []byte {
	if len(b) == 0 || b[len(b)-1] == '\n' {
		return b
	}
	return append(append([]byte(nil), b...), '\n')
}
