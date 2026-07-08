package market

// recipe_meta.go — the single source of asset metadata: parse it out of the
// signed install.ian, never accept it from the request body (server-server.md
// §3/§10.2). Reuses the production recipe parser (cmd/isann/recipe) so market
// and the CLI agree byte-for-byte on what a recipe declares.
//
//	min_isann ← `#pragma ISANN <ver>`
//	name / version / summary(or description) / license / tags ← `# key:` header

import (
	"fmt"
	"strings"

	"github.com/isannai/isann-servers/pkg/recipe"
)

// ParseRecipeMeta parses a recipe body and extracts the derived metadata.
// It requires name + version (the PK components live in the recipe). Returns
// an error the server maps to 400.
func ParseRecipeMeta(body string) (AssetMeta, error) {
	rc, err := recipe.Parse("install.ian", []byte(body))
	if err != nil {
		return AssetMeta{}, err
	}
	m := AssetMeta{MinIsann: rc.MinIsannVersion}
	get := func(keys ...string) string {
		for _, k := range keys {
			if v := rc.Meta[k]; v != "" {
				return v
			}
		}
		return ""
	}
	m.Name = get("name")
	m.Author = get("author")
	m.Version = get("version")
	m.Summary = get("summary", "description")
	m.License = get("license")
	if tags := get("tags"); tags != "" {
		for _, t := range strings.Split(tags, ",") {
			if t = strings.TrimSpace(t); t != "" {
				m.Tags = append(m.Tags, t)
			}
		}
	}
	if m.Name == "" {
		return m, fmt.Errorf("recipe missing `# name:` header")
	}
	if m.Version == "" {
		return m, fmt.Errorf("recipe missing `# version:` header")
	}
	return m, nil
}
