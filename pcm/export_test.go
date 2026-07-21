package pcm

// MaxConvertBatch exposes the conversion batch cap to the pcm_test package, so
// that a test asserting how much a converting Read returns can derive the
// number from the constant instead of restating it. A hardcoded copy would
// keep passing against a changed cap, or fail with a message describing the
// wrong cause.
const MaxConvertBatch = maxConvertBatch
