package market

// sqlstore.go — the single Store implementation, dialect-aware over SQLite
// (dev) and MySQL/MariaDB (prod). Mirrors the pkg/gate pattern (Store
// interface + SQL backend) but collapses the two drivers into one impl with a
// tiny dialect shim, since the query set is identical and only DDL + now()
// differ.
//
// The recipe body lives inline in asset_versions.recipe_body (central DB).
// Tags are stored comma-joined on assets.tags (schema.md sanctions this over a
// separate asset_tags table) and searched with LIKE.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"
)

// Sentinel errors mapped to HTTP codes by the server.
var (
	ErrNotFound      = errors.New("asset not found")
	ErrVersionExists = errors.New("version already exists")
)

type sqlStore struct {
	db     *sql.DB
	driver string // "sqlite" | "mysql"
}

// NewStore opens the DB for the given driver and runs migrations.
func NewStore(driver, dsn string) (Store, error) {
	var (
		db  *sql.DB
		err error
	)
	switch driver {
	case "sqlite", "":
		driver = "sqlite"
		db, err = sql.Open("sqlite", dsn+"?_journal=WAL&_busy_timeout=5000")
	case "mysql":
		db, err = sql.Open("mysql", dsn)
	default:
		return nil, fmt.Errorf("unsupported db driver: %s", driver)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", driver, err)
	}
	s := &sqlStore{db: db, driver: driver}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *sqlStore) Close() error { return s.db.Close() }

// now returns the dialect SQL expression for the current timestamp.
func (s *sqlStore) now() string {
	if s.driver == "mysql" {
		return "UTC_TIMESTAMP()"
	}
	return "datetime('now')"
}

func (s *sqlStore) migrate() error {
	var stmts []string
	if s.driver == "mysql" {
		stmts = []string{
			`CREATE TABLE IF NOT EXISTS authors (
				id           BIGINT AUTO_INCREMENT PRIMARY KEY,
				eoa          CHAR(42) NOT NULL UNIQUE,
				handle       VARCHAR(39) UNIQUE,
				display_name VARCHAR(100) DEFAULT '',
				created_at   DATETIME NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS assets (
				id             BIGINT AUTO_INCREMENT PRIMARY KEY,
				type           VARCHAR(16) NOT NULL,
				author_id      BIGINT NOT NULL,
				name           VARCHAR(100) NOT NULL,
				latest_version VARCHAR(32) DEFAULT '',
				summary        VARCHAR(255) DEFAULT '',
				license        VARCHAR(64) DEFAULT '',
				min_isann      VARCHAR(16) DEFAULT '',
				tags           VARCHAR(512) DEFAULT '',
				visibility     VARCHAR(8) NOT NULL DEFAULT 'private',
				price          VARCHAR(32) NOT NULL DEFAULT '0',
				token          VARCHAR(64),
				downloads      BIGINT NOT NULL DEFAULT 0,
				avg_rating     DECIMAL(3,2),
				review_count   INT NOT NULL DEFAULT 0,
				created_at     DATETIME NOT NULL,
				updated_at     DATETIME NOT NULL,
				UNIQUE KEY uq_asset (type, author_id, name),
				INDEX idx_type (type),
				INDEX idx_author (author_id)
			)`,
			`CREATE TABLE IF NOT EXISTS asset_versions (
				id          BIGINT AUTO_INCREMENT PRIMARY KEY,
				asset_id    BIGINT NOT NULL,
				version     VARCHAR(32) NOT NULL,
				sha256      CHAR(64) NOT NULL,
				recipe_body MEDIUMTEXT NOT NULL,
				created_at  DATETIME NOT NULL,
				UNIQUE KEY uq_version (asset_id, version)
			)`,
		}
	} else {
		stmts = []string{
			`CREATE TABLE IF NOT EXISTS authors (
				id           INTEGER PRIMARY KEY AUTOINCREMENT,
				eoa          TEXT NOT NULL UNIQUE,
				handle       TEXT UNIQUE,
				display_name TEXT DEFAULT '',
				created_at   TEXT NOT NULL DEFAULT (datetime('now'))
			)`,
			`CREATE TABLE IF NOT EXISTS assets (
				id             INTEGER PRIMARY KEY AUTOINCREMENT,
				type           TEXT NOT NULL,
				author_id      INTEGER NOT NULL,
				name           TEXT NOT NULL,
				latest_version TEXT DEFAULT '',
				summary        TEXT DEFAULT '',
				license        TEXT DEFAULT '',
				min_isann      TEXT DEFAULT '',
				tags           TEXT DEFAULT '',
				visibility     TEXT NOT NULL DEFAULT 'private',
				price          TEXT NOT NULL DEFAULT '0',
				token          TEXT,
				downloads      INTEGER NOT NULL DEFAULT 0,
				avg_rating     REAL,
				review_count   INTEGER NOT NULL DEFAULT 0,
				created_at     TEXT NOT NULL DEFAULT (datetime('now')),
				updated_at     TEXT NOT NULL DEFAULT (datetime('now')),
				UNIQUE(type, author_id, name)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_assets_type ON assets(type)`,
			`CREATE INDEX IF NOT EXISTS idx_assets_author ON assets(author_id)`,
			`CREATE TABLE IF NOT EXISTS asset_versions (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				asset_id    INTEGER NOT NULL,
				version     TEXT NOT NULL,
				sha256      TEXT NOT NULL,
				recipe_body TEXT NOT NULL,
				created_at  TEXT NOT NULL DEFAULT (datetime('now')),
				UNIQUE(asset_id, version)
			)`,
		}
	}
	for _, ddl := range stmts {
		if _, err := s.db.Exec(ddl); err != nil {
			return fmt.Errorf("ddl: %w", err)
		}
	}
	return nil
}

// authorID resolves (creating if needed) the authors row for an EOA. The EOA
// is stored lowercase — it is the natural identity key recovered from a
// signature. tx-free: pushes are serialized enough at v1 scale, and the UNIQUE
// on eoa makes a lost race a benign re-select.
func (s *sqlStore) authorID(ctx context.Context, eoa string) (int64, error) {
	eoa = strings.ToLower(eoa)
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM authors WHERE eoa = ?`, eoa).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO authors(eoa, created_at) VALUES(?, `+s.now()+`)`, eoa)
	if err != nil {
		// Lost the race — another push created it. Re-select.
		if e2 := s.db.QueryRowContext(ctx, `SELECT id FROM authors WHERE eoa = ?`, eoa).Scan(&id); e2 == nil {
			return id, nil
		}
		return 0, err
	}
	return res.LastInsertId()
}

// --- reads ------------------------------------------------------------------

const assetCols = `a.type, au.eoa, COALESCE(au.handle,''), a.name, a.latest_version,
	a.summary, a.license, a.min_isann, a.tags, a.visibility, a.price, a.token,
	a.downloads, a.avg_rating, a.created_at, a.updated_at`

// scanAsset reads one row selected with assetCols into an Asset. The display
// author is the handle when set (8004 alias), else the EOA.
func (s *sqlStore) scanAsset(sc interface{ Scan(...any) error }) (Asset, error) {
	var (
		a                Asset
		eoa, handle      string
		token            sql.NullString
		rating           sql.NullFloat64
		tags             string
		created, updated string
	)
	if err := sc.Scan(&a.Type, &eoa, &handle, &a.Name, &a.Version, &a.Summary,
		&a.License, &a.MinIsann, &tags, &a.Visibility, &a.Price, &token,
		&a.Downloads, &rating, &created, &updated); err != nil {
		return a, err
	}
	a.Eoa = eoa
	a.Handle = handle
	a.Author = eoa
	if handle != "" {
		a.Author = handle
	}
	a.Ref = a.Author + "/" + a.Name + "@" + a.Version
	if tags != "" {
		a.Tags = strings.Split(tags, ",")
	}
	if token.Valid {
		a.Token = &token.String
	}
	if rating.Valid {
		a.Rating = &rating.Float64
	}
	a.CreatedAt = parseTime(created)
	a.UpdatedAt = parseTime(updated)
	return a, nil
}

func (s *sqlStore) ListAssets(ctx context.Context, f ListFilter) ([]Asset, int, error) {
	return s.list(ctx, f, "", true)
}

func (s *sqlStore) ListMine(ctx context.Context, authorEOA string, f ListFilter) ([]Asset, int, error) {
	return s.list(ctx, f, strings.ToLower(authorEOA), false)
}

// list backs both the public browse (publicOnly=true) and /mine (mineEOA set,
// publicOnly=false). Exactly one of the two modes is used per call.
func (s *sqlStore) list(ctx context.Context, f ListFilter, mineEOA string, publicOnly bool) ([]Asset, int, error) {
	var conds []string
	var args []any
	if publicOnly {
		conds = append(conds, "a.visibility = 'public'")
	}
	if mineEOA != "" {
		conds = append(conds, "au.eoa = ?")
		args = append(args, mineEOA)
	}
	if f.Type != "" {
		conds = append(conds, "a.type = ?")
		args = append(args, f.Type)
	}
	if f.Author != "" {
		conds = append(conds, "(au.eoa = ? OR au.handle = ?)")
		args = append(args, strings.ToLower(f.Author), f.Author)
	}
	if f.Query != "" {
		like := "%" + strings.ToLower(f.Query) + "%"
		conds = append(conds, "(LOWER(a.name) LIKE ? OR LOWER(a.summary) LIKE ? OR LOWER(a.tags) LIKE ?)")
		args = append(args, like, like, like)
	}
	if f.Paid != nil {
		if *f.Paid {
			conds = append(conds, "a.price <> '0'")
		} else {
			conds = append(conds, "a.price = '0'")
		}
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM assets a JOIN authors au ON a.author_id = au.id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	order := "a.downloads DESC"
	switch f.Sort {
	case "updated":
		order = "a.updated_at DESC"
	case "rating":
		order = "a.avg_rating DESC"
	}
	page, limit := f.Page, f.Limit
	if page < 1 {
		page = 1
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	q := `SELECT ` + assetCols + ` FROM assets a JOIN authors au ON a.author_id = au.id` +
		where + ` ORDER BY ` + order + ` LIMIT ? OFFSET ?`
	args = append(args, limit, (page-1)*limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Asset{}
	for rows.Next() {
		a, err := s.scanAsset(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, a)
	}
	return items, total, rows.Err()
}

func (s *sqlStore) GetAsset(ctx context.Context, typ, authorEOA, name, version string) (*Asset, []Version, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+assetCols+` FROM assets a JOIN authors au ON a.author_id = au.id
		 WHERE a.type = ? AND au.eoa = ? AND a.name = ?`,
		typ, strings.ToLower(authorEOA), name)
	a, err := s.scanAsset(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	versions, err := s.ListVersions(ctx, typ, authorEOA, name)
	if err != nil {
		return nil, nil, err
	}
	// Pin the requested version in the returned Asset (else latest).
	if version != "" {
		found := false
		for _, v := range versions {
			if v.Version == version {
				a.Version = version
				a.Ref = a.Author + "/" + a.Name + "@" + version
				found = true
				break
			}
		}
		if !found {
			return nil, nil, nil
		}
	}
	return &a, versions, nil
}

func (s *sqlStore) ListVersions(ctx context.Context, typ, authorEOA, name string) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT v.version, v.sha256, v.created_at
		 FROM asset_versions v
		 JOIN assets a ON v.asset_id = a.id
		 JOIN authors au ON a.author_id = au.id
		 WHERE a.type = ? AND au.eoa = ? AND a.name = ?`,
		typ, strings.ToLower(authorEOA), name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Version
	for rows.Next() {
		var v Version
		var created string
		if err := rows.Scan(&v.Version, &v.SHA256, &created); err != nil {
			return nil, err
		}
		v.CreatedAt = parseTime(created)
		list = append(list, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortVersionsDesc(list)
	return list, nil
}

func (s *sqlStore) GetRecipe(ctx context.Context, typ, authorEOA, name, version string) (string, string, string, bool, error) {
	// Resolve latest when version omitted.
	var visibility string
	if version == "" {
		err := s.db.QueryRowContext(ctx,
			`SELECT a.latest_version, a.visibility FROM assets a JOIN authors au ON a.author_id = au.id
			 WHERE a.type = ? AND au.eoa = ? AND a.name = ?`,
			typ, strings.ToLower(authorEOA), name).Scan(&version, &visibility)
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", "", false, nil
		}
		if err != nil {
			return "", "", "", false, err
		}
	}
	var body, sha256 string
	err := s.db.QueryRowContext(ctx,
		`SELECT v.recipe_body, v.sha256, a.visibility
		 FROM asset_versions v
		 JOIN assets a ON v.asset_id = a.id
		 JOIN authors au ON a.author_id = au.id
		 WHERE a.type = ? AND au.eoa = ? AND a.name = ? AND v.version = ?`,
		typ, strings.ToLower(authorEOA), name, version).Scan(&body, &sha256, &visibility)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, err
	}
	return body, sha256, visibility, true, nil
}

// --- writes -----------------------------------------------------------------

func (s *sqlStore) PutAsset(ctx context.Context, in PushInput) (*Asset, error) {
	authorID, err := s.authorID(ctx, in.Author)
	if err != nil {
		return nil, err
	}
	// Ensure the asset row exists (INSERT if new). We can't rely on dialect
	// upsert here, so select-or-insert.
	var assetID int64
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM assets WHERE type = ? AND author_id = ? AND name = ?`,
		in.Type, authorID, in.Meta.Name).Scan(&assetID)
	if errors.Is(err, sql.ErrNoRows) {
		res, ierr := s.db.ExecContext(ctx,
			`INSERT INTO assets(type, author_id, name, latest_version, summary, license, min_isann, tags, visibility, price, created_at, updated_at)
			 VALUES(?,?,?,?,?,?,?,?, 'private', '0', `+s.now()+`, `+s.now()+`)`,
			in.Type, authorID, in.Meta.Name, in.Meta.Version, in.Meta.Summary,
			in.Meta.License, in.Meta.MinIsann, strings.Join(in.Meta.Tags, ","))
		if ierr != nil {
			return nil, ierr
		}
		assetID, _ = res.LastInsertId()
	} else if err != nil {
		return nil, err
	}

	// Insert the (immutable) version. Duplicate = 409.
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO asset_versions(asset_id, version, sha256, recipe_body, created_at)
		 VALUES(?,?,?,?, `+s.now()+`)`,
		assetID, in.Meta.Version, in.SHA256, in.RecipeBody)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrVersionExists
		}
		return nil, err
	}

	// Recompute latest_version across all versions + refresh metadata to the
	// newly-pushed version's parsed values (latest wins for display).
	if err := s.refreshLatest(ctx, assetID, in.Meta); err != nil {
		return nil, err
	}
	a, _, err := s.GetAsset(ctx, in.Type, in.Author, in.Meta.Name, "")
	return a, err
}

// refreshLatest sets assets.latest_version to the max semver among its versions
// and, when the just-pushed version IS the new latest, copies its metadata onto
// the asset row (summary/license/tags/min_isann are latest-version derived).
func (s *sqlStore) refreshLatest(ctx context.Context, assetID int64, meta AssetMeta) error {
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM asset_versions WHERE asset_id = ?`, assetID)
	if err != nil {
		return err
	}
	var latest string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		if latest == "" || semverLess(latest, v) {
			latest = v
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if latest == meta.Version {
		_, err = s.db.ExecContext(ctx,
			`UPDATE assets SET latest_version = ?, summary = ?, license = ?, min_isann = ?, tags = ?, updated_at = `+s.now()+` WHERE id = ?`,
			latest, meta.Summary, meta.License, meta.MinIsann, strings.Join(meta.Tags, ","), assetID)
	} else {
		_, err = s.db.ExecContext(ctx,
			`UPDATE assets SET latest_version = ?, updated_at = `+s.now()+` WHERE id = ?`, latest, assetID)
	}
	return err
}

func (s *sqlStore) SetVisibility(ctx context.Context, typ, authorEOA, name, visibility string) (bool, error) {
	id, cur, err := s.assetIDVis(ctx, typ, authorEOA, name)
	if err != nil {
		return false, err
	}
	if cur == visibility {
		return false, nil // idempotent skip
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE assets SET visibility = ?, updated_at = `+s.now()+` WHERE id = ?`, visibility, id)
	return err == nil, err
}

func (s *sqlStore) SetPrice(ctx context.Context, typ, authorEOA, name, price string, token *string) (bool, error) {
	id, _, err := s.assetIDVis(ctx, typ, authorEOA, name)
	if err != nil {
		return false, err
	}
	var curPrice string
	var curToken sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT price, token FROM assets WHERE id = ?`, id).Scan(&curPrice, &curToken); err != nil {
		return false, err
	}
	newToken := ""
	if token != nil {
		newToken = *token
	}
	if curPrice == price && curToken.String == newToken {
		return false, nil // idempotent skip
	}
	var tokArg any
	if token != nil {
		tokArg = *token
	} else {
		tokArg = nil
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE assets SET price = ?, token = ?, updated_at = `+s.now()+` WHERE id = ?`, price, tokArg, id)
	return err == nil, err
}

func (s *sqlStore) SetMeta(ctx context.Context, typ, authorEOA, name string, tags, summary *string) (bool, error) {
	id, _, err := s.assetIDVis(ctx, typ, authorEOA, name)
	if err != nil {
		return false, err
	}
	var curTags, curSummary string
	if err := s.db.QueryRowContext(ctx, `SELECT tags, summary FROM assets WHERE id = ?`, id).Scan(&curTags, &curSummary); err != nil {
		return false, err
	}
	var sets []string
	var args []any
	if tags != nil && *tags != curTags {
		sets = append(sets, "tags = ?")
		args = append(args, *tags)
	}
	if summary != nil && *summary != curSummary {
		sets = append(sets, "summary = ?")
		args = append(args, *summary)
	}
	if len(sets) == 0 {
		return false, nil // idempotent skip
	}
	sets = append(sets, "updated_at = "+s.now())
	args = append(args, id)
	_, err = s.db.ExecContext(ctx, `UPDATE assets SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	return err == nil, err
}

func (s *sqlStore) DeleteAsset(ctx context.Context, typ, authorEOA, name string) (bool, error) {
	id, _, err := s.assetIDVis(ctx, typ, authorEOA, name)
	if errors.Is(err, ErrNotFound) {
		return false, nil // idempotent skip
	}
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM asset_versions WHERE asset_id = ?`, id); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM assets WHERE id = ?`, id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *sqlStore) IncrDownloads(ctx context.Context, typ, authorEOA, name string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE assets SET downloads = downloads + 1
		 WHERE id = (SELECT a.id FROM assets a JOIN authors au ON a.author_id = au.id
		             WHERE a.type = ? AND au.eoa = ? AND a.name = ?)`,
		typ, strings.ToLower(authorEOA), name)
	return err
}

// assetIDVis returns the asset row id + current visibility, or ErrNotFound.
func (s *sqlStore) assetIDVis(ctx context.Context, typ, authorEOA, name string) (int64, string, error) {
	var id int64
	var vis string
	err := s.db.QueryRowContext(ctx,
		`SELECT a.id, a.visibility FROM assets a JOIN authors au ON a.author_id = au.id
		 WHERE a.type = ? AND au.eoa = ? AND a.name = ?`,
		typ, strings.ToLower(authorEOA), name).Scan(&id, &vis)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	return id, vis, err
}

// --- helpers ----------------------------------------------------------------

func isUniqueViolation(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate")
}

// parseTime accepts both the SQLite "2006-01-02 15:04:05" layout and RFC3339
// (some MySQL configs). Returns zero time on failure.
func parseTime(s string) time.Time {
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339, "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// semverLess reports whether a < b under a lax dotted-numeric ordering
// (1.2 < 1.10). Non-numeric components fall back to string compare.
func semverLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var xa, xb string
		if i < len(pa) {
			xa = pa[i]
		}
		if i < len(pb) {
			xb = pb[i]
		}
		na, ea := strconv.Atoi(xa)
		nb, eb := strconv.Atoi(xb)
		if ea == nil && eb == nil {
			if na != nb {
				return na < nb
			}
			continue
		}
		if xa != xb {
			return xa < xb
		}
	}
	return false
}

func sortVersionsDesc(vs []Version) {
	for i := 1; i < len(vs); i++ {
		for j := i; j > 0 && semverLess(vs[j-1].Version, vs[j].Version); j-- {
			vs[j-1], vs[j] = vs[j], vs[j-1]
		}
	}
}
