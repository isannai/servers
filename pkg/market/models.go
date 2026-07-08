package market

// models.go — market data model (API §10.2 / db-schema.md).
//
// An asset is keyed by the composite (type, author, name); versions hang off
// it 1:N. In the v1 CENTRAL model the recipe body (install.ian, KB) is stored
// inline per version — the DB is the payload store, not just an indexer. The
// db-schema.md CID/chain_tx columns are the Phase-3 DApp (IPFS + chain) shape
// and are left nullable/empty here.

import "time"

// AssetType enumerates the seven layer-2 asset kinds. Part of the primary key.
type AssetType = string

// ValidTypes is the closed enum for {type} (API §10.1). `package` is a bundle
// .ian (published via `recipe push -install`) — the only type `market install`
// runs; the rest are received via per-noun `pull`.
var ValidTypes = map[string]bool{
	"preset": true, "skill": true, "profile": true,
	"recipe": true, "app": true, "model": true, "mesh": true,
	"package": true,
}

// Asset is the name-unit record (one row per (type, author, name)). The
// metadata fields (summary/license/min_isann/tags/latest version) are DERIVED
// from the recipe at push time — never accepted from the request body.
type Asset struct {
	Type       string    `json:"type"`
	Author     string    `json:"author"`           // display identity: handle if registered, else EOA
	Eoa        string    `json:"eoa,omitempty"`    // canonical lowercase 0x… address (always)
	Handle     string    `json:"handle,omitempty"` // username (8004 alias); empty when unregistered
	Name       string    `json:"name"`
	Version    string    `json:"version"` // latest (list/meta convenience)
	Ref        string    `json:"ref"`     // {author}/{name}@{version}
	Summary    string    `json:"summary,omitempty"`
	License    string    `json:"license,omitempty"`
	MinIsann   string    `json:"min_isann,omitempty"`
	Tags       []string  `json:"tags,omitempty"`
	Visibility string    `json:"visibility,omitempty"` // public | private (write/mine responses only)
	Price      string    `json:"price"`                // decimal string; "0" = free
	Token      *string   `json:"token"`                // ERC-20 symbol/addr; nil = free
	Downloads  int64     `json:"downloads"`
	Rating     *float64  `json:"rating"` // avg (verified); nil when none / free
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Version is one immutable published version of an asset.
type Version struct {
	Version   string    `json:"version"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
}

// AssetMeta is the derived metadata parsed out of a recipe at push time.
// (See recipe_meta.go.) name/version drive the PK; the rest are display.
type AssetMeta struct {
	Name     string
	Author   string // from `# author:` if present; provenance authority is the signature
	Version  string
	Summary  string
	License  string
	MinIsann string
	Tags     []string
}

// PushInput is everything the store needs to persist one push.
type PushInput struct {
	Type       string
	Author     string // lowercase EOA recovered from the signature
	Meta       AssetMeta
	RecipeBody string
	SHA256     string
}

// ListFilter carries the GET /v1/assets query knobs.
type ListFilter struct {
	Type   string
	Author string // EOA or handle
	Query  string // keyword over name/summary/tags
	Paid   *bool  // nil = all
	Sort   string // downloads | updated | rating
	Page   int
	Limit  int
}
