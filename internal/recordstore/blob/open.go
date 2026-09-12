package blob

import (
	"context"
	"fmt"
	"net/url"

	"github.com/jackc/pgx/v5/pgxpool"

	gcblob "gocloud.dev/blob"

	// Blank imports register each scheme with gocloud.dev/blob's default
	// URL mux, so gcblob.OpenBucket recognizes it.
	_ "gocloud.dev/blob/azureblob"
	_ "gocloud.dev/blob/fileblob"
	_ "gocloud.dev/blob/gcsblob"
	_ "gocloud.dev/blob/s3blob"
)

// schemePGBlob is the URL scheme for the in-repo Postgres driver. The
// scheme is the whole URL: the process builds the connection from
// VARROA_RECORDSTORE_DSN, so no rendered manifest holds a database
// credential.
const schemePGBlob = "pgblob"

// supportedSchemesMessage names every scheme Open accepts.
const supportedSchemesMessage = "pgblob, s3, gs, azblob, file"

// Open opens a Store from a URL. Scheme "pgblob" opens the Postgres
// driver over pool and ignores the rest of the URL. Schemes "s3", "gs",
// "azblob", and "file" open a gocloud.dev bucket through
// gcblob.OpenBucket and ignore pool. Every other scheme returns an error
// naming the supported set.
func Open(ctx context.Context, rawURL string, pool *pgxpool.Pool) (Store, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse blob store url: %w", err)
	}

	if err := checkScheme(u.Scheme); err != nil {
		return nil, err
	}

	if u.Scheme == schemePGBlob {
		if pool == nil {
			return nil, fmt.Errorf("blob: pgblob scheme needs a database pool")
		}
		return NewPGBlobStore(pool), nil
	}

	bucket, err := gcblob.OpenBucket(ctx, rawURL)
	if err != nil {
		return nil, fmt.Errorf("open bucket %q: %w", rawURL, err)
	}
	return NewBucketStore(bucket), nil
}

// checkScheme reports an error naming the supported schemes when scheme
// is not one Open accepts. It runs before Open does any I/O, so a caller
// can check scheme routing without opening a real bucket.
func checkScheme(scheme string) error {
	switch scheme {
	case schemePGBlob, "s3", "gs", "azblob", "file":
		return nil
	default:
		return fmt.Errorf("blob: unsupported scheme %q; supported schemes are %s", scheme, supportedSchemesMessage)
	}
}
