package consensus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseS3Path splits an operator-supplied S3 URI into a bucket and a key
// prefix. Getting this wrong writes misbehaviour reports to the wrong bucket,
// or to the bucket root, where nobody looks for them.
func TestParseS3Path(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		wantBucket string
		wantPrefix string
		wantErr    string
	}{
		{
			name:       "bucket only",
			path:       "s3://my-bucket",
			wantBucket: "my-bucket",
			wantPrefix: "",
		},
		{
			name:       "bucket with a trailing slash",
			path:       "s3://my-bucket/",
			wantBucket: "my-bucket",
			wantPrefix: "",
		},
		{
			name:       "a prefix without a trailing slash gets one",
			path:       "s3://my-bucket/misbehaviors",
			wantBucket: "my-bucket",
			wantPrefix: "misbehaviors/",
		},
		{
			name:       "a prefix that already ends in a slash is left alone",
			path:       "s3://my-bucket/misbehaviors/",
			wantBucket: "my-bucket",
			wantPrefix: "misbehaviors/",
		},
		{
			name:       "a nested prefix keeps every segment",
			path:       "s3://my-bucket/erpc/consensus/misbehaviors",
			wantBucket: "my-bucket",
			wantPrefix: "erpc/consensus/misbehaviors/",
		},
		{
			name:    "a missing scheme is rejected",
			path:    "my-bucket/misbehaviors",
			wantErr: "must start with s3://",
		},
		{
			name:    "another scheme is rejected",
			path:    "gs://my-bucket/misbehaviors",
			wantErr: "must start with s3://",
		},
		{
			name:    "an empty string is rejected",
			path:    "",
			wantErr: "must start with s3://",
		},
		{
			name:    "a scheme with no bucket is rejected",
			path:    "s3://",
			wantErr: "no bucket specified",
		},
		{
			name: "a leading slash after the scheme is rejected",
			// s3:///prefix would otherwise parse to an empty bucket and quietly
			// write nowhere.
			path:    "s3:///misbehaviors",
			wantErr: "no bucket specified",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bucket, prefix, err := parseS3Path(tc.path)

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Equal(t, "", bucket)
				assert.Equal(t, "", prefix)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantBucket, bucket)
			assert.Equal(t, tc.wantPrefix, prefix)
		})
	}
}

// TestSanitizeForFilename proves every character that breaks a path or an S3
// key is replaced. A network id such as "evm:1" would otherwise create a
// directory boundary on some filesystems and a nested key on S3.
func TestSanitizeForFilename(t *testing.T) {
	assert.Equal(t, "evm_1", sanitizeForFilename("evm:1"))
	assert.Equal(t, "a_b", sanitizeForFilename("a/b"))
	assert.Equal(t, "a_b", sanitizeForFilename(`a\b`))
	assert.Equal(t, "a_b", sanitizeForFilename("a b"))
	assert.Equal(t, "a_b_c_d", sanitizeForFilename("a\nb\rc\td"))
	assert.Equal(t, "a_b_c_d_e_f", sanitizeForFilename(`a*b?c"d<e>f`))
	assert.Equal(t, "a_b", sanitizeForFilename("a|b"))

	// Characters that are safe must survive untouched, or every filename
	// becomes an unreadable run of underscores.
	assert.Equal(t, "eth_getLogs-2024.01.02", sanitizeForFilename("eth_getLogs-2024.01.02"))
	assert.Equal(t, "", sanitizeForFilename(""))
}
