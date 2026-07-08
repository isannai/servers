package market

import "context"

// Store is the market persistence interface. One implementation (sqlStore)
// backs both SQLite (dev) and MySQL/MariaDB (prod) via a dialect switch, the
// same way pkg/gate exposes a Store interface over two drivers.
type Store interface {
	// ListAssets returns public assets (latest version per row) matching the
	// filter, plus the total count for pagination.
	ListAssets(ctx context.Context, f ListFilter) (items []Asset, total int, err error)

	// ListMine returns ALL assets (public + private) for one author EOA.
	ListMine(ctx context.Context, authorEOA string, f ListFilter) (items []Asset, total int, err error)

	// GetAsset returns one asset at a specific version ("" = latest). Includes
	// the version list. Returns (nil,nil) when absent.
	GetAsset(ctx context.Context, typ, authorEOA, name, version string) (*Asset, []Version, error)

	// ListVersions returns the version list (newest first). nil when absent.
	ListVersions(ctx context.Context, typ, authorEOA, name string) ([]Version, error)

	// GetRecipe returns the install.ian body + sha256 for a version ("" =
	// latest), along with the asset visibility so the caller can gate private
	// reads. found=false when the asset/version is absent.
	GetRecipe(ctx context.Context, typ, authorEOA, name, version string) (body, sha256, visibility string, found bool, err error)

	// PutAsset inserts a new version (creating the asset row on first push).
	// Published versions are immutable: a duplicate (type,author,name,version)
	// returns ErrVersionExists. New pushes default to private.
	PutAsset(ctx context.Context, in PushInput) (*Asset, error)

	// SetVisibility flips public/private. changed=false when already that state
	// (idempotent skip). Returns ErrNotFound when the asset is absent.
	SetVisibility(ctx context.Context, typ, authorEOA, name, visibility string) (changed bool, err error)

	// SetPrice sets the pricing axis. price="0" (token nil) = free. changed=false
	// when already at that price (idempotent skip).
	SetPrice(ctx context.Context, typ, authorEOA, name, price string, token *string) (changed bool, err error)

	// SetMeta edits the mutable discovery metadata (tags, summary) on the asset
	// row WITHOUT touching versions — the recipe/content stays immutable. Each
	// arg is nil to leave that field unchanged. changed=false = idempotent skip.
	// A later push re-derives these from the new version's recipe.
	SetMeta(ctx context.Context, typ, authorEOA, name string, tags, summary *string) (changed bool, err error)

	// DeleteAsset removes an asset and all versions. existed=false when it was
	// already absent (idempotent skip).
	DeleteAsset(ctx context.Context, typ, authorEOA, name string) (existed bool, err error)

	// IncrDownloads bumps the download counter (best-effort; errors ignored by
	// callers so a counter hiccup never fails an install).
	IncrDownloads(ctx context.Context, typ, authorEOA, name string) error

	Close() error
}
