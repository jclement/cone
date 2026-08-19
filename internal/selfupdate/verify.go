// Sigstore verification: is this checksums.txt the one our release workflow signed?
//
// The release is signed keylessly — GitHub Actions presents an OIDC token, Fulcio issues a
// certificate valid for ten minutes that names the workflow, and the signature plus that
// certificate go into Rekor. There is no long-lived key to trust, so what gets checked is the
// identity inside the certificate: it must be a workflow in github.com/jclement/cone, vouched
// for by the sigstore trust root that TUF hands us.
//
// Two bundle formats exist in the wild and which one a release carries depends on the cosign
// version CI installed: cosign's own JSON (base64Signature/cert/rekorBundle — this is what
// v0.1.0 shipped) and the protobuf Sigstore bundle. Both are verified in full here. Neither
// path may end in "close enough": a failure at any step is a refusal to install.

package selfupdate

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tlog"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

const (
	// Who is allowed to have signed it: GitHub's OIDC issuer, and a SAN naming a workflow
	// in this repo. The anchor on the pattern is what stops a perfectly valid certificate
	// for somebody else's repository from passing.
	oidcIssuer      = "https://token.actions.githubusercontent.com"
	identityPattern = `^https://github\.com/` + repo + `/`
)

// verifySignature checks that checksums is the artifact the release workflow signed, using
// whichever bundle format the release carries.
func verifySignature(checksums, bundleJSON []byte) error {
	var legacy cosignBundle
	if err := json.Unmarshal(bundleJSON, &legacy); err == nil && legacy.Base64Signature != "" {
		return verifyCosignBundle(checksums, &legacy)
	}
	return verifySigstoreBundle(checksums, bundleJSON)
}

// trustRoot is sigstore's trust root, fetched over TUF. Going to the network for it is what
// makes verification survive key rotation at Fulcio or Rekor without a cone release.
func trustRoot() (*root.LiveTrustedRoot, error) {
	tr, err := root.NewLiveTrustedRoot(tuf.DefaultOptions())
	if err != nil {
		return nil, fmt.Errorf("fetching the sigstore trust root: %w", err)
	}
	return tr, nil
}

// signerIdentity is the certificate identity a cone release must carry.
func signerIdentity() (verify.CertificateIdentity, error) {
	id, err := verify.NewShortCertificateIdentity(oidcIssuer, "", "", identityPattern)
	if err != nil {
		return id, fmt.Errorf("building the expected signer identity: %w", err)
	}
	return id, nil
}

// ---- protobuf Sigstore bundle ----

func verifySigstoreBundle(checksums, bundleJSON []byte) error {
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return fmt.Errorf("parsing the bundle: %w", err)
	}
	tr, err := trustRoot()
	if err != nil {
		return err
	}
	verifier, err := verify.NewVerifier(tr,
		verify.WithSignedCertificateTimestamps(1), // the certificate was logged to a CT log
		verify.WithTransparencyLog(1),             // the signature is in Rekor
		verify.WithObserverTimestamps(1),          // and it was made while the cert was valid
	)
	if err != nil {
		return fmt.Errorf("building the verifier: %w", err)
	}
	signer, err := signerIdentity()
	if err != nil {
		return err
	}
	_, err = verifier.Verify(&b, verify.NewPolicy(
		verify.WithArtifact(bytes.NewReader(checksums)),
		verify.WithCertificateIdentity(signer),
	))
	return err
}

// ---- cosign's own bundle format ----

// cosignBundle is what `cosign sign-blob --bundle` writes: the signature, the Fulcio
// certificate, and the Rekor entry with the log's countersignature over it.
type cosignBundle struct {
	Base64Signature string `json:"base64Signature"`
	Cert            string `json:"cert"` // base64 of the PEM
	RekorBundle     struct {
		SignedEntryTimestamp string `json:"SignedEntryTimestamp"`
		Payload              struct {
			Body           string `json:"body"`
			IntegratedTime int64  `json:"integratedTime"`
			LogIndex       int64  `json:"logIndex"`
			LogID          string `json:"logID"`
		} `json:"Payload"`
	} `json:"rekorBundle"`
}

// verifyCosignBundle does by hand what the protobuf verifier does for the newer format, in
// the same order and to the same standard: the log entry is genuine, it is about these exact
// bytes, the certificate is a real Fulcio certificate that was valid when the log saw it, it
// names our workflow, and it signed this artifact.
func verifyCosignBundle(checksums []byte, b *cosignBundle) error {
	sig, err := base64.StdEncoding.DecodeString(b.Base64Signature)
	if err != nil {
		return fmt.Errorf("decoding the signature: %w", err)
	}
	cert, err := parseCert(b.Cert)
	if err != nil {
		return err
	}
	tr, err := trustRoot()
	if err != nil {
		return err
	}

	signedAt, err := verifyLogEntry(b, checksums, sig, tr)
	if err != nil {
		return err
	}

	// Fulcio certificates live for ten minutes, so "was it valid?" is only answerable
	// against the moment Rekor witnessed the signature — hence the order.
	chained := false
	for _, ca := range tr.FulcioCertificateAuthorities() {
		if _, err := ca.Verify(cert, signedAt); err == nil {
			chained = true
			break
		}
	}
	if !chained {
		return fmt.Errorf("the signing certificate was not issued by a trusted Fulcio CA")
	}

	summary, err := certificate.SummarizeCertificate(cert)
	if err != nil {
		return fmt.Errorf("reading the signing certificate: %w", err)
	}
	signer, err := signerIdentity()
	if err != nil {
		return err
	}
	if err := signer.Verify(summary); err != nil {
		return fmt.Errorf("the signer is not a %s release workflow: %w", repo, err)
	}

	if err := cert.CheckSignature(x509.ECDSAWithSHA256, checksums, sig); err != nil {
		return fmt.Errorf("the signature does not cover this checksums.txt: %w", err)
	}
	return nil
}

// verifyLogEntry checks Rekor's countersignature over the log entry and that the entry is
// about this artifact — without that second half, a genuine entry for something else could be
// borrowed to make an expired certificate look like it was valid at signing time. It returns
// the time the log witnessed the signature.
func verifyLogEntry(b *cosignBundle, checksums, sig []byte, tr *root.LiveTrustedRoot) (time.Time, error) {
	payload := b.RekorBundle.Payload
	body, err := base64.StdEncoding.DecodeString(payload.Body)
	if err != nil {
		return time.Time{}, fmt.Errorf("decoding the rekor entry: %w", err)
	}
	logID, err := hex.DecodeString(payload.LogID)
	if err != nil {
		return time.Time{}, fmt.Errorf("decoding the rekor log id: %w", err)
	}
	set, err := base64.StdEncoding.DecodeString(b.RekorBundle.SignedEntryTimestamp)
	if err != nil {
		return time.Time{}, fmt.Errorf("decoding the rekor entry timestamp: %w", err)
	}

	// NewEntry is deprecated in favour of the protobuf constructor, but this bundle format
	// only ever holds a Rekor v1 entry, which is exactly what it parses.
	entry, err := tlog.NewEntry(body, payload.IntegratedTime, payload.LogIndex, logID, set, nil)
	if err != nil {
		return time.Time{}, fmt.Errorf("reading the rekor entry: %w", err)
	}
	if err := tlog.VerifySET(entry, tr.RekorLogs()); err != nil {
		return time.Time{}, fmt.Errorf("rekor did not countersign this entry: %w", err)
	}

	digest, algorithm, ok := entry.GetHashedRekordDigest()
	if !ok {
		return time.Time{}, fmt.Errorf("the rekor entry is not a hashedrekord over an artifact")
	}
	want := sha256.Sum256(checksums)
	if algorithm != "sha256" || !bytes.Equal(digest, want[:]) {
		return time.Time{}, fmt.Errorf("the rekor entry is about a different artifact")
	}
	if !bytes.Equal(entry.Signature(), sig) {
		return time.Time{}, fmt.Errorf("the rekor entry holds a different signature")
	}
	return entry.IntegratedTime(), nil
}

func parseCert(base64PEM string) (*x509.Certificate, error) {
	der, err := base64.StdEncoding.DecodeString(base64PEM)
	if err != nil {
		return nil, fmt.Errorf("decoding the signing certificate: %w", err)
	}
	block, _ := pem.Decode(der)
	if block == nil {
		return nil, fmt.Errorf("the signing certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing the signing certificate: %w", err)
	}
	return cert, nil
}
