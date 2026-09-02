package provider

// ChecksumSHA256SelfComputed is the ChecksumAlgo label used by providers
// whose protocol exposes no trustworthy content hash of its own, so they
// hash the upload stream themselves (io.TeeReader over the body) and later
// verify by re-downloading and rehashing.
//
// This constant is shared rather than redeclared per provider because the
// value is a persisted, cross-package contract: it is written into
// upload_log by internal/history and handed back to the same provider's
// VerifyChecksum later, which rejects any algo label it does not recognize.
// Three independent copies of the literal (which is how this started) meant
// a typo in a fourth would silently produce history rows that can never be
// verified - a data-integrity bug with no error message.
//
// Providers with a real server-side hash deliberately do NOT use this and
// declare their own native label instead ("md5" for Google Drive, "sha1"
// for Backblaze B2), because for them verification is a cheap metadata read
// rather than a full re-download. The label is opaque to the core either
// way - see ChecksumVerifier.
const ChecksumSHA256SelfComputed = "sha256-self-computed"
