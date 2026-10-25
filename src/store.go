package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/fs"
	"modernc.org/sqlite"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ---------------- store: SQLite metadata --------------------

type DB struct {
	db *sql.DB
	mu sync.Mutex
}

// idBase36 returns a short unique id.
var idSeq int = 0
var idMtx sync.Mutex

// newID returns a short unique id (base36 of a monotonic per-process counter +
// a random suffix) used to distinguish media files recorded within the same
// second. Timestamps in filenames are only second-granular, so the id must stay
// unique within that window — but it is kept short for clean file names.
func newID() string {
	idMtx.Lock()
	idSeq++
	var seq = idSeq
	idMtx.Unlock()
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	var r = binary.BigEndian.Uint32(buf[:])
	return strconv.FormatInt(int64(seq), 36) + strconv.FormatInt(int64(r), 36)
}

// friendlyTS returns a human-readable local timestamp (YYYYMMDD-HHMMSS) for use
// in media filenames, e.g. 20260824-155900.
func friendlyTS(unix int64) string {
	return time.Unix(unix, 0).Local().Format("20060102-150405")
}

func openDB(path string) (*DB, error) {
	var cstr = "file:" + path
	conn, cerr := sqlite.NewConnector(cstr)
	if cerr != nil {
		return nil, cerr
	}
	var store DB
	store.db = sql.OpenDB(conn)
	// Simultaneous cameras mean simultaneous writers. PRAGMAs like busy_timeout
	// are per-connection while database/sql pools connections, so serialize
	// everything through one connection: concurrent INSERT/UPDATEs wait
	// instead of failing with SQLITE_BUSY (which would drop detections and
	// strand grouping slots). WAL keeps external readers unblocked.
	// NOTE: CSV exports therefore paginate in short cursors (never one cursor
	// across the whole HTTP response) so a long export cannot starve writers.
	store.db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", "PRAGMA synchronous=NORMAL"} {
		if _, err := store.db.Exec(pragma); err != nil {
			logIt(LWARN, "db pragma failed", map[string]string{"pragma": pragma, "error": err.Error()})
		}
	}
	// The DB stores camera credentials (rtsp_url/user/password) in plaintext:
	// keep it owner-only.
	_ = os.Chmod(path, 0o600)
	_, errr := store.db.Exec("CREATE TABLE IF NOT EXISTS cameras(id TEXT PRIMARY KEY, name TEXT, enabled INTEGER, onvif_url TEXT, rtsp_url TEXT, username TEXT, password TEXT, ptz_available INTEGER, created_at INTEGER)")
	if errr != nil {
		return nil, errr
	}
	_, errr = store.db.Exec("CREATE TABLE IF NOT EXISTS detections(id TEXT PRIMARY KEY, camera_id TEXT, name TEXT, det_type TEXT, confidence REAL, started_at INTEGER, ended_at INTEGER, snapshot_path TEXT, clip_path TEXT, uploaded INTEGER DEFAULT 0, extra_snapshots TEXT, locked INTEGER DEFAULT 0, s3_expected INTEGER DEFAULT 0, size_bytes INTEGER DEFAULT 0)")
	if errr != nil {
		return nil, errr
	}
	// Optional migration for pre-existing databases (no-op when the column exists).
	for _, mig := range []string{
		"ALTER TABLE detections ADD COLUMN extra_snapshots TEXT",
		"ALTER TABLE detections ADD COLUMN locked INTEGER DEFAULT 0",
		"ALTER TABLE detections ADD COLUMN s3_expected INTEGER DEFAULT 0",
		"ALTER TABLE detections ADD COLUMN size_bytes INTEGER DEFAULT 0",
		"ALTER TABLE detections ADD COLUMN vision_json TEXT DEFAULT ''",
		"ALTER TABLE detections ADD COLUMN vision_count INTEGER DEFAULT 0",
		"ALTER TABLE detections ADD COLUMN vision_top TEXT DEFAULT ''",
		"ALTER TABLE detections ADD COLUMN vision_ms REAL DEFAULT 0",
		"ALTER TABLE detections ADD COLUMN vision_error TEXT DEFAULT ''",
		"ALTER TABLE detections ADD COLUMN vision_at INTEGER DEFAULT 0",
		"ALTER TABLE detections ADD COLUMN vision_snap TEXT DEFAULT ''",
		"ALTER TABLE detections ADD COLUMN bbox_json TEXT DEFAULT ''",
		// Merged camera tags per detection (e.g. "motion,person"). det_type
		// stays the primary/most-specific tag for filters/retention.
		"ALTER TABLE detections ADD COLUMN det_types TEXT DEFAULT ''",
	} {
		if _, err := store.db.Exec(mig); err != nil {
			logIt(LDEBUG, "db migration skipped", map[string]string{"sql": mig, "error": err.Error()})
		}
	}
	// One YOLO result per snapshot file (snap_index 0 = primary, 1..N =
	// burst extras). The detections.vision_* columns stay as the BEST-result
	// summary (back-compat for MQTT/filters/badges); details live here.
	_, errr = store.db.Exec("CREATE TABLE IF NOT EXISTS vision_results(detection_id TEXT, snap_index INTEGER, snap_path TEXT, vision_json TEXT DEFAULT '', vision_count INTEGER DEFAULT 0, vision_top TEXT DEFAULT '', vision_ms REAL DEFAULT 0, vision_total_ms REAL DEFAULT 0, vision_error TEXT DEFAULT '', vision_at INTEGER DEFAULT 0, PRIMARY KEY(detection_id, snap_index))")
	if errr != nil {
		return nil, errr
	}
	_, _ = store.db.Exec("ALTER TABLE vision_results ADD COLUMN vision_total_ms REAL DEFAULT 0")
	// Backfill from the legacy single-row summary (no-op when already filled).
	_, _ = store.db.Exec("INSERT OR IGNORE INTO vision_results(detection_id, snap_index, snap_path, vision_json, vision_count, vision_top, vision_ms, vision_error, vision_at) SELECT id, 0, COALESCE(vision_snap,''), COALESCE(vision_json,''), COALESCE(vision_count,0), COALESCE(vision_top,''), COALESCE(vision_ms,0), COALESCE(vision_error,''), COALESCE(vision_at,0) FROM detections WHERE COALESCE(vision_at,0) > 0")
	_, errr = store.db.Exec("CREATE TABLE IF NOT EXISTS onvif_events(id TEXT PRIMARY KEY, camera_id TEXT, camera_name TEXT, det_type TEXT, created_at INTEGER, detection_id TEXT)")
	if errr != nil {
		return nil, errr
	}
	_, errr = store.db.Exec("CREATE TABLE IF NOT EXISTS vision_overrides(detection_id TEXT, snap_index INTEGER, label TEXT, created_at INTEGER, PRIMARY KEY(detection_id, snap_index, label))")
	if errr != nil {
		return nil, errr
	}
	_, _ = store.db.Exec("ALTER TABLE onvif_events ADD COLUMN detection_id TEXT")
	_, _ = store.db.Exec("ALTER TABLE onvif_events ADD COLUMN bbox_json TEXT DEFAULT ''")
	_, _ = store.db.Exec("CREATE INDEX IF NOT EXISTS idx_detections_started ON detections(started_at)")
	_, _ = store.db.Exec("CREATE INDEX IF NOT EXISTS idx_detections_camera ON detections(camera_id, started_at)")
	_, _ = store.db.Exec("CREATE INDEX IF NOT EXISTS idx_onvif_events_detection ON onvif_events(detection_id)")
	backfillDetectionTypes(&store)
	return &store, nil
}

// backfillDetectionTypes fills det_types for rows created before the column
// existed, from their linked raw events (fallback: det_type). It deliberately
// does not rewrite det_type: existing recordings keep their original primary
// type, only the merged tag list is recovered. One-time no-op afterwards.
func backfillDetectionTypes(store *DB) {
	type pending struct {
		id, primary, events string
	}
	var rows []pending
	var rs, err = store.db.Query(`SELECT d.id, COALESCE(d.det_type,''), COALESCE((SELECT GROUP_CONCAT(DISTINCT e.det_type) FROM onvif_events e WHERE e.detection_id = d.id), '') FROM detections d WHERE COALESCE(d.det_types,'') = ''`)
	if err != nil {
		return
	}
	for rs.Next() {
		var p pending
		if serr := rs.Scan(&p.id, &p.primary, &p.events); serr == nil {
			rows = append(rows, p)
		}
	}
	rs.Close()
	// Rows are collected before updating: the pool has a single connection, so
	// writing while the cursor is open would deadlock.
	for _, p := range rows {
		var merged = mergeDetTypeList(p.events, p.primary)
		if merged == "" {
			continue
		}
		_, _ = store.db.Exec("UPDATE detections SET det_types=? WHERE id=?", merged, p.id)
	}
}

func (s *DB) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// ---------------- store: query layer ------------------------

func dbUpsertCam(store *DB, cam *CameraCfg, id string, ptz int) error {
	var q = "INSERT INTO cameras(id,name,enabled,onvif_url,rtsp_url,username,password,ptz_available,created_at) VALUES(?,?,?,?,?,?,?,?,?)" +
		" ON CONFLICT(id) DO UPDATE SET name=excluded.name, enabled=excluded.enabled, onvif_url=excluded.onvif_url, rtsp_url=excluded.rtsp_url, username=excluded.username, password=excluded.password, ptz_available=excluded.ptz_available"
	_, e := store.db.Exec(q, id, cam.Name, cam.Enabled, cam.Onvif.Url, cam.RtspURL, cam.Onvif.Username, cam.Onvif.Password, ptz, time.Now().Unix())
	return e
}

func dbInsertDetection(store *DB, d *Detection) error {
	// Single-use statement: Exec directly instead of Prepare (a Prepare would
	// need an explicit Close to avoid leaking statement handles).
	// NOTE: legacy s3_snapshot_key/s3_clip_key columns may exist in old DBs;
	// they are intentionally unwritten (always empty) and unread.
	var detTypes = d.detTypes
	if strings.TrimSpace(detTypes) == "" {
		detTypes = d.detType
	}
	_, e := store.db.Exec("INSERT INTO detections(id, camera_id, name, det_type, confidence, started_at, ended_at, snapshot_path, clip_path, uploaded, s3_expected, size_bytes, bbox_json, det_types) VALUES(?,?,?,?,?,?,?,?,?,0,?,?,?,?)", d.id, d.cameraID, d.camName, d.detType, d.confidence, d.startedAt, d.endedAt, d.snapshotPath, d.clipPath, d.s3Expected, d.sizeBytes, d.bboxJSON, detTypes)
	return e
}

// ---------------- detection tags (merged group types) -------------

// normalizeDetType lowercases/trims a tag and strips commas so the
// comma-separated det_types column can never be corrupted.
func normalizeDetType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	return strings.ReplaceAll(t, ",", "")
}

// parseDetTypes splits a stored det_types value into individual tags.
func parseDetTypes(raw string) []string {
	var out []string
	for _, t := range strings.Split(raw, ",") {
		if t = normalizeDetType(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// mergeDetTypeList appends every tag from adds (each may itself be a
// comma-separated list) to existing, de-duplicating and preserving first-seen
// order. Returns a comma-separated list.
func mergeDetTypeList(existing string, adds ...string) string {
	var out []string
	var seen = map[string]bool{}
	var addOne = func(raw string) {
		var t = normalizeDetType(raw)
		if t == "" || seen[t] {
			return
		}
		seen[t] = true
		out = append(out, t)
	}
	for _, t := range parseDetTypes(existing) {
		addOne(t)
	}
	for _, add := range adds {
		for _, t := range parseDetTypes(add) {
			addOne(t)
		}
	}
	return strings.Join(out, ",")
}

// detTypeRank orders tags by specificity for primary-type promotion:
// person/vehicle/pet > other > motion. Manual media never gets promoted.
func detTypeRank(t string) int {
	t = normalizeDetType(t)
	if strings.HasPrefix(t, "manual-") {
		return 9
	}
	switch t {
	case "person", "vehicle", "pet":
		return 3
	case "other":
		return 2
	case "motion":
		return 1
	}
	return 0
}

// primaryDetType picks the most specific tag from list. Ties keep the current
// fallback (or the first equally-specific entry), so the first specific tag
// seen stays primary.
func primaryDetType(types []string, fallback string) string {
	var best = normalizeDetType(fallback)
	var bestRank = detTypeRank(best)
	for _, t := range types {
		if r := detTypeRank(t); r > bestRank {
			best, bestRank = t, r
		}
	}
	if best == "" && len(types) > 0 {
		best = types[0]
	}
	return best
}

// detectionTags returns the detection's full tag list with the primary tag
// first, falling back to detType for rows created before det_types existed.
func detectionTags(d *Detection) []string {
	if d == nil {
		return nil
	}
	var out []string
	var seen = map[string]bool{}
	var add = func(raw string) {
		var t = normalizeDetType(raw)
		if t == "" || seen[t] {
			return
		}
		seen[t] = true
		out = append(out, t)
	}
	add(d.detType)
	for _, t := range parseDetTypes(d.detTypes) {
		add(t)
	}
	return out
}

func dbSetUploaded(store *DB, id string) error {
	_, e := store.db.Exec("UPDATE detections SET uploaded=1 WHERE id=?", id)
	return e
}

// dbSetClipPath records a detection's clip after it has been assembled.
func dbSetClipPath(store *DB, id string, path string) error {
	_, e := store.db.Exec("UPDATE detections SET clip_path=? WHERE id=?", path, id)
	return e
}

// bboxMergeMtx serializes read-modify-write on detections.bbox_json so two
// concurrent extension events can't lose each other's boxes.
var bboxMergeMtx sync.Mutex

// dbMergeBBox appends the given boxes (JSON array) to a detection's stored
// boxes, de-duplicating and capping the list at 32 entries.
func dbMergeBBox(store *DB, id, addJSON string) error {
	if strings.TrimSpace(addJSON) == "" {
		return nil
	}
	bboxMergeMtx.Lock()
	defer bboxMergeMtx.Unlock()
	var cur string
	_ = store.db.QueryRow("SELECT COALESCE(bbox_json,'') FROM detections WHERE id=?", id).Scan(&cur)
	var merged = append(parseBoxesJSON(cur), parseBoxesJSON(addJSON)...)
	var seen = map[string]bool{}
	var out []detBox
	for _, b := range merged {
		var k = fmt.Sprintf("%s|%.3f|%.3f|%.3f|%.3f", b.Type, b.X1, b.Y1, b.X2, b.Y2)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, b)
		if len(out) >= 32 {
			break
		}
	}
	_, e := store.db.Exec("UPDATE detections SET bbox_json=? WHERE id=?", boxesJSON(out), id)
	return e
}

// detTypeMergeMtx serializes the read-modify-write merge of det_types/det_type
// so concurrent extension events cannot lose each other's tags.
var detTypeMergeMtx sync.Mutex

// dbMergeDetectionType adds dtype to a detection's merged tag list and promotes
// its primary type when dtype is more specific. found is false when the row
// does not exist yet (the reserving collectDetection is still capturing
// snapshots): callers keep the merge in the in-memory grouping slot instead.
func dbMergeDetectionType(store *DB, id, dtype string) (found bool, promoted bool, err error) {
	var add = normalizeDetType(dtype)
	if add == "" {
		return false, false, nil
	}
	detTypeMergeMtx.Lock()
	defer detTypeMergeMtx.Unlock()
	var curPrimary, curTypes string
	err = store.db.QueryRow("SELECT COALESCE(det_type,''), COALESCE(det_types,'') FROM detections WHERE id=?", id).Scan(&curPrimary, &curTypes)
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	var merged = mergeDetTypeList(curTypes, add)
	var primary = primaryDetType(parseDetTypes(merged), curPrimary)
	var before = detTypeRank(curPrimary)
	if merged == curTypes && primary == curPrimary {
		return true, false, nil
	}
	_, err = store.db.Exec("UPDATE detections SET det_types=?, det_type=? WHERE id=?", merged, primary, id)
	return true, detTypeRank(primary) > before, err
}

func dbSetLocked(store *DB, id string, locked bool) error {
	var v = 0
	if locked {
		v = 1
	}
	_, e := store.db.Exec("UPDATE detections SET locked=? WHERE id=?", v, id)
	return e
}

// getDetection returns one detection by id, or nil.
func getDetection(store *DB, id string) (*Detection, error) {
	var q = "SELECT id, camera_id, name, det_type, confidence, started_at, ended_at, snapshot_path, clip_path, extra_snapshots, locked, uploaded, s3_expected, size_bytes, vision_json, vision_count, vision_top, vision_ms, vision_error, vision_at, bbox_json, det_types FROM detections WHERE id=?"
	var rs, e = store.db.Query(q, id)
	if e != nil {
		return nil, e
	}
	var out *Detection
	if rs.Next() {
		var d Detection
		var locked sql.NullInt64
		var uploaded sql.NullInt64
		var expected sql.NullInt64
		var sizeB sql.NullInt64
		var extra sql.NullString
		var vjson sql.NullString
		var vcount sql.NullInt64
		var vtop sql.NullString
		var vms sql.NullFloat64
		var verr sql.NullString
		var vat sql.NullInt64
		var bbox sql.NullString
		var dtypes sql.NullString
		if serr := rs.Scan(&d.id, &d.cameraID, &d.camName, &d.detType, &d.confidence, &d.startedAt, &d.endedAt, &d.snapshotPath, &d.clipPath, &extra, &locked, &uploaded, &expected, &sizeB, &vjson, &vcount, &vtop, &vms, &verr, &vat, &bbox, &dtypes); serr != nil {
			rs.Close()
			return nil, serr
		}
		if extra.Valid {
			d.extraSnap = extra.String
		}
		if locked.Valid && locked.Int64 != 0 {
			d.locked = 1
		}
		if uploaded.Valid && uploaded.Int64 != 0 {
			d.uploaded = 1
		}
		if expected.Valid && expected.Int64 != 0 {
			d.s3Expected = 1
		}
		if sizeB.Valid {
			d.sizeBytes = sizeB.Int64
		}
		applyVisionScan(&d, vjson, vcount, vtop, vms, verr, vat)
		if bbox.Valid {
			d.bboxJSON = bbox.String
		}
		if dtypes.Valid {
			d.detTypes = dtypes.String
		}
		out = &d
	}
	rs.Close()
	if rerr := rs.Err(); rerr != nil {
		return nil, rerr
	}
	return out, nil
}

// listAllDetections returns every detection row (no LIMIT) for administrative
// operations like delete-all that must not silently skip old rows.
func listAllDetections(store *DB) ([]Detection, error) {
	var out []Detection
	var q = "SELECT id, camera_id, name, det_type, confidence, started_at, ended_at, snapshot_path, clip_path, extra_snapshots, locked, uploaded, s3_expected, size_bytes, vision_json, vision_count, vision_top, vision_ms, vision_error, vision_at, bbox_json, det_types FROM detections ORDER BY started_at DESC"
	var rs, e = store.db.Query(q)
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	for rs.Next() {
		var d Detection
		var locked sql.NullInt64
		var uploaded sql.NullInt64
		var expected sql.NullInt64
		var sizeB sql.NullInt64
		var extra sql.NullString
		var vjson sql.NullString
		var vcount sql.NullInt64
		var vtop sql.NullString
		var vms sql.NullFloat64
		var verr sql.NullString
		var vat sql.NullInt64
		var bbox sql.NullString
		var dtypes sql.NullString
		if serr := rs.Scan(&d.id, &d.cameraID, &d.camName, &d.detType, &d.confidence, &d.startedAt, &d.endedAt, &d.snapshotPath, &d.clipPath, &extra, &locked, &uploaded, &expected, &sizeB, &vjson, &vcount, &vtop, &vms, &verr, &vat, &bbox, &dtypes); serr != nil {
			return nil, serr
		}
		if extra.Valid {
			d.extraSnap = extra.String
		}
		if locked.Valid && locked.Int64 != 0 {
			d.locked = 1
		}
		if uploaded.Valid && uploaded.Int64 != 0 {
			d.uploaded = 1
		}
		if expected.Valid && expected.Int64 != 0 {
			d.s3Expected = 1
		}
		if sizeB.Valid {
			d.sizeBytes = sizeB.Int64
		}
		applyVisionScan(&d, vjson, vcount, vtop, vms, verr, vat)
		if bbox.Valid {
			d.bboxJSON = bbox.String
		}
		if dtypes.Valid {
			d.detTypes = dtypes.String
		}
		out = append(out, d)
	}
	if rerr := rs.Err(); rerr != nil {
		return nil, rerr
	}
	return out, nil
}

// pruneEmptyDirs removes empty directories bottom-up under root (leftover
// date/camera folders after deletions). Files are never touched.
func pruneEmptyDirs(root string) {
	var dirs []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != root {
			dirs = append(dirs, p)
		}
		return nil
	})
	// deepest first so parents become empty and get removed too
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, dir := range dirs {
		_ = os.Remove(dir) // only succeeds when empty
	}
}

// deleteDetectionFiles removes all media (snapshot, clip, extra snapshots)
// associated with a detection row, both on local disk and in S3 when enabled.
func deleteDetectionFiles(app *App, d *Detection) {
	if d == nil {
		return
	}
	var s3paths []string
	var del = func(p string) {
		if p == "" {
			return
		}
		if _, e := os.Stat(p); e == nil {
			if rerr := os.Remove(p); rerr == nil {
				logIt(LDEBUG, "file deleted", map[string]string{"path": p})
			}
		}
		s3paths = append(s3paths, p)
	}
	del(d.snapshotPath)
	del(d.clipPath)
	if d.extraSnap != "" {
		for _, p := range strings.Split(d.extraSnap, ",") {
			del(p)
		}
	}
	logIt(LINFO, "detection media deleted", map[string]string{"id": d.id, "files": strconv.Itoa(len(s3paths))})
	deleteS3Files(app, s3paths)
}

// likeEscape neutralizes LIKE wildcards in a user-supplied substring filter so
// "%" and "_" match literally instead of acting as wildcards.
func likeEscape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "%", "\\%")
	s = strings.ReplaceAll(s, "_", "\\_")
	return s
}

// listDetectionsFiltered extends listDetections with vision filters:
// hasVision=true keeps only rows with vision_count>0, label filters on
// vision_top/vision_json (substring, case-insensitive handled by caller).
// limit/offset paginate (limit<=0 = default 500, capped at 5000) so rows past
// the first page stay reachable; countDetectionsFiltered returns the total.
func listDetectionsFiltered(store *DB, from int64, to int64, camera string, dtype string, label string, hasVision bool, limit int, offset int) ([]Detection, error) {
	var out []Detection
	var q = "SELECT id, camera_id, name, det_type, confidence, started_at, ended_at, snapshot_path, clip_path, extra_snapshots, locked, uploaded, s3_expected, size_bytes, vision_json, vision_count, vision_top, vision_ms, vision_error, vision_at, bbox_json, det_types FROM detections"
	var cond []string
	if from > 0 {
		cond = append(cond, "started_at >= ?")
	}
	if to > 0 {
		cond = append(cond, "started_at <= ?")
	}
	if camera != "" {
		cond = append(cond, "(camera_id = ? OR name = ?)")
	}
	if dtype != "" {
		cond = append(cond, "(det_type = ? OR instr(','||COALESCE(det_types,'')||',', ','||?||',') > 0)")
	}
	if hasVision {
		cond = append(cond, "vision_count > 0")
	}
	if label != "" {
		cond = append(cond, `(vision_top LIKE ? ESCAPE '\' OR vision_json LIKE ? ESCAPE '\')`)
	}
	if len(cond) > 0 {
		q += " WHERE " + strings.Join(cond, " AND ")
	}
	if limit <= 0 {
		limit = 500
	}
	if limit > 5000 {
		limit = 5000
	}
	if offset < 0 {
		offset = 0
	}
	q += " ORDER BY started_at DESC LIMIT ? OFFSET ?"
	var args []any
	if from > 0 {
		args = append(args, from)
	}
	if to > 0 {
		args = append(args, to)
	}
	if camera != "" {
		args = append(args, camera, camera)
	}
	if dtype != "" {
		args = append(args, dtype, dtype)
	}
	if label != "" {
		var like = "%" + likeEscape(label) + "%"
		args = append(args, like, like)
	}
	args = append(args, limit, offset)
	var rs, e = store.db.Query(q, args...)
	if e != nil {
		return nil, e
	}
	for rs.Next() {
		var d Detection
		var locked sql.NullInt64
		var uploaded sql.NullInt64
		var expected sql.NullInt64
		var sizeB sql.NullInt64
		var extra sql.NullString
		var vjson sql.NullString
		var vcount sql.NullInt64
		var vtop sql.NullString
		var vms sql.NullFloat64
		var verr sql.NullString
		var vat sql.NullInt64
		var bbox sql.NullString
		var dtypes sql.NullString
		if serr := rs.Scan(&d.id, &d.cameraID, &d.camName, &d.detType, &d.confidence, &d.startedAt, &d.endedAt, &d.snapshotPath, &d.clipPath, &extra, &locked, &uploaded, &expected, &sizeB, &vjson, &vcount, &vtop, &vms, &verr, &vat, &bbox, &dtypes); serr != nil {
			rs.Close()
			return nil, serr
		}
		if extra.Valid {
			d.extraSnap = extra.String
		}
		if locked.Valid && locked.Int64 != 0 {
			d.locked = 1
		}
		if uploaded.Valid && uploaded.Int64 != 0 {
			d.uploaded = 1
		}
		if expected.Valid && expected.Int64 != 0 {
			d.s3Expected = 1
		}
		if sizeB.Valid {
			d.sizeBytes = sizeB.Int64
		}
		applyVisionScan(&d, vjson, vcount, vtop, vms, verr, vat)
		if bbox.Valid {
			d.bboxJSON = bbox.String
		}
		if dtypes.Valid {
			d.detTypes = dtypes.String
		}
		out = append(out, d)
	}
	rs.Close()
	if rerr := rs.Err(); rerr != nil {
		return nil, rerr
	}
	return out, nil
}

// countDetectionsFiltered returns the total rows matching the same filters as
// listDetectionsFiltered (for the X-Total-Count header / pagination).
func countDetectionsFiltered(store *DB, from int64, to int64, camera string, dtype string, label string, hasVision bool) int64 {
	var q = "SELECT COUNT(*) FROM detections"
	var cond []string
	if from > 0 {
		cond = append(cond, "started_at >= ?")
	}
	if to > 0 {
		cond = append(cond, "started_at <= ?")
	}
	if camera != "" {
		cond = append(cond, "(camera_id = ? OR name = ?)")
	}
	if dtype != "" {
		cond = append(cond, "(det_type = ? OR instr(','||COALESCE(det_types,'')||',', ','||?||',') > 0)")
	}
	if hasVision {
		cond = append(cond, "vision_count > 0")
	}
	if label != "" {
		cond = append(cond, `(vision_top LIKE ? ESCAPE '\' OR vision_json LIKE ? ESCAPE '\')`)
	}
	if len(cond) > 0 {
		q += " WHERE " + strings.Join(cond, " AND ")
	}
	var args []any
	if from > 0 {
		args = append(args, from)
	}
	if to > 0 {
		args = append(args, to)
	}
	if camera != "" {
		args = append(args, camera, camera)
	}
	if dtype != "" {
		args = append(args, dtype, dtype)
	}
	if label != "" {
		var like = "%" + likeEscape(label) + "%"
		args = append(args, like, like)
	}
	return dbCount(store, q, args...)
}

// ---------------- onvif_events: raw events -------------------

type OnvifEvent struct {
	id          string
	cameraID    string
	camName     string
	detType     string
	createdAt   int64
	detectionID string
	// bbox is the RTSP SEI box(es) that triggered this raw event (JSON, "" for
	// ONVIF events which carry no coordinates).
	bbox string
}

func dbInsertOnvifEvent(store *DB, e *OnvifEvent) error {
	_, err := store.db.Exec("INSERT INTO onvif_events(id, camera_id, camera_name, det_type, created_at, detection_id, bbox_json) VALUES(?,?,?,?,?,?,?)", e.id, e.cameraID, e.camName, e.detType, e.createdAt, e.detectionID, e.bbox)
	return err
}

func listOnvifEventsForDetection(store *DB, detID string) ([]OnvifEvent, error) {
	var out []OnvifEvent
	if store == nil || store.db == nil || detID == "" {
		return out, nil
	}
	rs, e := store.db.Query("SELECT id, camera_id, camera_name, det_type, created_at, detection_id, bbox_json FROM onvif_events WHERE detection_id=? ORDER BY created_at ASC LIMIT 500", detID)
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	for rs.Next() {
		var ev OnvifEvent
		var did, bbox sql.NullString
		if serr := rs.Scan(&ev.id, &ev.cameraID, &ev.camName, &ev.detType, &ev.createdAt, &did, &bbox); serr != nil {
			return nil, serr
		}
		if did.Valid {
			ev.detectionID = did.String
		}
		if bbox.Valid {
			ev.bbox = bbox.String
		}
		out = append(out, ev)
	}
	return out, rs.Err()
}

func dbApplyVisionExclusion(store *DB, detID string, snapIndex int, overrides map[int][]string) error {
	if store == nil || store.db == nil {
		return fmt.Errorf("nil store")
	}
	labels, ok := overrides[snapIndex]
	if !ok || len(labels) == 0 {
		return nil
	}
	excluded := map[string]bool{}
	for _, l := range labels {
		excluded[strings.ToLower(strings.TrimSpace(l))] = true
	}
	vres, err := dbListVisionResults(store, detID)
	if err != nil {
		return err
	}
	for i := range vres {
		if vres[i].snapIndex != snapIndex {
			continue
		}
		filtered := visionFilterExcluded(vres[i].json, excluded)
		cnt, top := visionSummaryForJSON(filtered)
		if _, err := store.db.Exec("UPDATE vision_results SET vision_json=?, vision_count=?, vision_top=? WHERE detection_id=? AND snap_index=?", filtered, cnt, top, detID, snapIndex); err != nil {
			return err
		}
		if snapIndex == 0 {
			if _, err := store.db.Exec("UPDATE detections SET vision_json=?, vision_count=?, vision_top=? WHERE id=?", filtered, cnt, top, detID); err != nil {
				return err
			}
		}
	}
	return nil
}

func listOnvifEvents(store *DB, from int64, to int64, camera string, dtype string) ([]OnvifEvent, error) {
	var out []OnvifEvent
	var q = "SELECT id, camera_id, camera_name, det_type, created_at, detection_id, bbox_json FROM onvif_events"
	var cond []string
	if from > 0 {
		cond = append(cond, "created_at >= ?")
	}
	if to > 0 {
		cond = append(cond, "created_at <= ?")
	}
	if camera != "" {
		cond = append(cond, "(camera_id = ? OR camera_name = ?)")
	}
	if dtype != "" {
		cond = append(cond, "det_type = ?")
	}
	if len(cond) > 0 {
		q += " WHERE " + strings.Join(cond, " AND ")
	}
	q += " ORDER BY created_at DESC LIMIT 1000"
	var args []any
	if from > 0 {
		args = append(args, from)
	}
	if to > 0 {
		args = append(args, to)
	}
	if camera != "" {
		args = append(args, camera, camera)
	}
	if dtype != "" {
		args = append(args, dtype)
	}
	var rs, e = store.db.Query(q, args...)
	if e != nil {
		return nil, e
	}
	for rs.Next() {
		var ev OnvifEvent
		var detID sql.NullString
		var bbox sql.NullString
		if serr := rs.Scan(&ev.id, &ev.cameraID, &ev.camName, &ev.detType, &ev.createdAt, &detID, &bbox); serr != nil {
			rs.Close()
			return nil, serr
		}
		if detID.Valid {
			ev.detectionID = detID.String
		}
		if bbox.Valid {
			ev.bbox = bbox.String
		}
		out = append(out, ev)
	}
	rs.Close()
	if rerr := rs.Err(); rerr != nil {
		return nil, rerr
	}
	return out, nil
}

func onvifEventJSON(ev *OnvifEvent) string {
	var arr []string
	arr = append(arr, kv("id", ev.id))
	arr = append(arr, kv("camera_id", ev.cameraID))
	arr = append(arr, kv("camera_name", ev.camName))
	arr = append(arr, kv("type", ev.detType))
	arr = append(arr, "\"created_at\":"+fmt.Sprintf("%d", ev.createdAt))
	arr = append(arr, "\"started_at\":"+fmt.Sprintf("%d", ev.createdAt))
	arr = append(arr, kv("detection_id", ev.detectionID))
	// Compatibility fields for reuse in event list UI
	arr = append(arr, kv("det_type", ev.detType))
	// alias for grouped event id
	arr = append(arr, kv("event_id", ev.id))
	arr = append(arr, kv("group_id", ev.detectionID))
	if bbox, ok := sanitizeBboxJSON(ev.bbox); ok {
		arr = append(arr, `"bbox":`+bbox)
	} else {
		arr = append(arr, `"bbox":null`)
	}
	return "{" + strings.Join(arr, ",") + "}"
}

// ---------------- detect: model --------------------------------

type Detection struct {
	id       string
	cameraID string
	camName  string
	detType  string
	// detTypes is the comma-separated list of every camera tag merged into
	// this group (e.g. "motion,person"), in first-seen order. detType remains
	// the primary (most specific) tag; empty on rows created before the column
	// existed, in which case readers fall back to detType.
	detTypes     string
	confidence   float64
	startedAt    int64
	endedAt      int64
	snapshotPath string
	clipPath     string
	extraSnap    string
	locked       int
	uploaded     int
	// s3Expected is 1 when S3 upload was enabled at detection time (i.e. this
	// row should eventually be uploaded); rows created while S3 was off stay 0
	// so the pending counter only tracks genuinely outstanding uploads.
	s3Expected int
	// sizeBytes caches the total media size (snapshot+extras+clip). 0 means
	// unknown (old rows): readers compute it live instead.
	sizeBytes int64
	// Vision fields: optional YOLO analysis of the snapshot (external API).
	// visionJSON is the full detections array (JSON), visionCount its length,
	// visionTop the "label:conf" of the best box, visionMs the server-side
	// inference_ms, visionError the last failure (degraded-silent), visionAt
	// the unix time of the last analysis.
	visionJSON  string
	visionCount int
	visionTop   string
	visionMs    float64
	visionError string
	visionAt    int64
	// bboxJSON holds the RTSP SEI detection boxes observed for this detection
	// (JSON array of {type,x1,y1,x2,y2,score?}), normalized 0..1. Empty for
	// ONVIF-only detections.
	bboxJSON string
}

// fileSize returns the size of path or 0 when missing.
func fileSize(p string) int64 {
	if p == "" {
		return 0
	}
	if s, e := os.Stat(p); e == nil {
		return s.Size()
	}
	return 0
}

// detectionSize totals all media files currently attached to d.
func detectionSize(d *Detection) int64 {
	if d == nil {
		return 0
	}
	var total = fileSize(d.snapshotPath) + fileSize(d.clipPath)
	if d.extraSnap != "" {
		for _, p := range strings.Split(d.extraSnap, ",") {
			total += fileSize(p)
		}
	}
	return total
}

// dbSetSize records the cached total media size for a detection.
func dbSetSize(store *DB, id string, size int64) error {
	_, e := store.db.Exec("UPDATE detections SET size_bytes=? WHERE id=?", size, id)
	return e
}

// applyVisionScan fills the vision fields of d from nullable columns.
func applyVisionScan(d *Detection, vjson sql.NullString, vcount sql.NullInt64, vtop sql.NullString, vms sql.NullFloat64, verr sql.NullString, vat sql.NullInt64) {
	if d == nil {
		return
	}
	if vjson.Valid {
		d.visionJSON = vjson.String
	}
	if vcount.Valid {
		d.visionCount = int(vcount.Int64)
	}
	if vtop.Valid {
		d.visionTop = vtop.String
	}
	if vms.Valid {
		d.visionMs = vms.Float64
	}
	if verr.Valid {
		d.visionError = verr.String
	}
	if vat.Valid {
		d.visionAt = vat.Int64
	}
}

// VisionSnapResult is one YOLO outcome for one snapshot file of a detection
// (snapIndex 0 = primary, 1..N = burst extras in /snapshot/{n} order).
type VisionSnapResult struct {
	snapIndex int
	snapPath  string
	json      string
	count     int
	top       string
	ms        float64
	totalMs   float64
	err       string
	at        int64
}

func visionDetsJSON(dets []visionDetection) string {
	var raw []byte
	raw, _ = json.Marshal(dets)
	if string(raw) == "null" {
		raw = []byte("[]")
	}
	// Bound the stored JSON (boxes are small; 64KB cap is defensive). Byte
	// truncation would cut mid-JSON and break ?boxes=1, so drop whole boxes.
	if len(raw) > 65536 {
		var arr []json.RawMessage
		if json.Unmarshal(raw, &arr) == nil {
			// Empty (not nil) so dropping every box still marshals to "[]",
			// keeping the "always a JSON array" contract for consumers.
			kept := []json.RawMessage{}
			var size = 2 // "[]"
			for _, m := range arr {
				if size+len(m)+1 > 65536 {
					break
				}
				kept = append(kept, m)
				size += len(m) + 1
			}
			if out, merr := json.Marshal(kept); merr == nil && len(out) > 0 {
				raw = out
			}
		}
	}
	return string(raw)
}

// dbUpsertVisionResult records one per-snapshot YOLO outcome (success or
// failure) and refreshes the detection-level BEST-result summary.
func dbUpsertVisionResult(store *DB, id string, snapIndex int, snapPath string, res *visionResult, vserr string, at int64) error {
	if store == nil {
		return fmt.Errorf("nil store")
	}
	var dets []visionDetection
	var ms, totalMs float64
	if res != nil {
		dets = res.Detections
		ms = res.InferenceMs
		totalMs = res.TotalMs
	} else if prev, perr := dbGetVisionResult(store, id, snapPath); perr == nil && prev != nil && prev.count > 0 {
		// Never let an error (e.g. a "busy" overload mark) wipe a previous
		// success for this snapshot: keep the best boxes, record the error.
		dets = parseVisionDets(prev.json)
		ms = prev.ms
		totalMs = prev.totalMs
	}
	if len(vserr) > 300 {
		// Cut on a rune boundary: the error may carry the vision server's own
		// UTF-8 text and is later embedded raw in API/MQTT JSON, where a split
		// multi-byte rune is invalid UTF-8.
		var cut = 300
		for cut > 0 && !utf8.RuneStart(vserr[cut]) {
			cut--
		}
		vserr = vserr[:cut]
	}
	_, e := store.db.Exec("INSERT INTO vision_results(detection_id, snap_index, snap_path, vision_json, vision_count, vision_top, vision_ms, vision_total_ms, vision_error, vision_at) VALUES(?,?,?,?,?,?,?,?,?,?)"+
		" ON CONFLICT(detection_id, snap_index) DO UPDATE SET snap_path=excluded.snap_path, vision_json=excluded.vision_json, vision_count=excluded.vision_count, vision_top=excluded.vision_top, vision_ms=excluded.vision_ms, vision_total_ms=excluded.vision_total_ms, vision_error=excluded.vision_error, vision_at=excluded.vision_at",
		id, snapIndex, snapPath, visionDetsJSON(dets), len(dets), visionTopLabel(dets), ms, totalMs, vserr, at)
	if e != nil {
		return e
	}
	return dbRefreshVisionSummary(store, id)
}

// dbRefreshVisionSummary folds vision_results into the detections.vision_*
// summary columns: the BEST result wins (max count, tie = latest). The
// summary error is the latest error, shown only when the best is empty.
func dbRefreshVisionSummary(store *DB, id string) error {
	if store == nil {
		return fmt.Errorf("nil store")
	}
	var rows, err = dbListVisionResults(store, id)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	var best = &rows[0]
	for i := range rows {
		if rows[i].count > best.count || (rows[i].count == best.count && rows[i].at > best.at) {
			best = &rows[i]
		}
	}
	var latestErr string
	var latestAt int64
	for i := range rows {
		if rows[i].at >= latestAt && rows[i].err != "" {
			latestAt = rows[i].at
			latestErr = rows[i].err
		}
	}
	var sumErr string
	if best.count == 0 {
		sumErr = latestErr
	}
	_, e := store.db.Exec("UPDATE detections SET vision_json=?, vision_count=?, vision_top=?, vision_ms=?, vision_error=?, vision_at=?, vision_snap=? WHERE id=?",
		best.json, best.count, best.top, best.ms, sumErr, best.at, best.snapPath, id)
	return e
}

// dbListVisionResults returns every per-snapshot outcome for a detection,
// ordered by snap_index.
func dbListVisionResults(store *DB, id string) ([]VisionSnapResult, error) {
	var out []VisionSnapResult
	if store == nil {
		return out, fmt.Errorf("nil store")
	}
	var rs, e = store.db.Query("SELECT snap_index, snap_path, vision_json, vision_count, vision_top, vision_ms, vision_total_ms, vision_error, vision_at FROM vision_results WHERE detection_id=? ORDER BY snap_index ASC", id)
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	for rs.Next() {
		var r VisionSnapResult
		var vjson, vtop, verr, vpath sql.NullString
		var vcount sql.NullInt64
		var vms, vtotal sql.NullFloat64
		var vat sql.NullInt64
		if serr := rs.Scan(&r.snapIndex, &vpath, &vjson, &vcount, &vtop, &vms, &vtotal, &verr, &vat); serr != nil {
			return nil, serr
		}
		if vpath.Valid {
			r.snapPath = vpath.String
		}
		if vjson.Valid {
			r.json = vjson.String
		}
		if vcount.Valid {
			r.count = int(vcount.Int64)
		}
		if vtop.Valid {
			r.top = vtop.String
		}
		if vms.Valid {
			r.ms = vms.Float64
		}
		if vtotal.Valid {
			r.totalMs = vtotal.Float64
		}
		if verr.Valid {
			r.err = verr.String
		}
		if vat.Valid {
			r.at = vat.Int64
		}
		out = append(out, r)
	}
	if rerr := rs.Err(); rerr != nil {
		return nil, rerr
	}
	return out, nil
}

// dbGetVisionResult returns the outcome for one exact snapshot file, or nil.
func dbGetVisionResult(store *DB, id string, snapPath string) (*VisionSnapResult, error) {
	if store == nil || snapPath == "" {
		return nil, nil
	}
	var r VisionSnapResult
	var vjson, vtop, verr, vpath sql.NullString
	var vcount sql.NullInt64
	var vms, vtotal sql.NullFloat64
	var vat sql.NullInt64
	var rs, e = store.db.Query("SELECT snap_index, snap_path, vision_json, vision_count, vision_top, vision_ms, vision_total_ms, vision_error, vision_at FROM vision_results WHERE detection_id=? AND snap_path=? LIMIT 1", id, snapPath)
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	if !rs.Next() {
		return nil, nil
	}
	if serr := rs.Scan(&r.snapIndex, &vpath, &vjson, &vcount, &vtop, &vms, &vtotal, &verr, &vat); serr != nil {
		return nil, serr
	}
	if vpath.Valid {
		r.snapPath = vpath.String
	}
	if vjson.Valid {
		r.json = vjson.String
	}
	if vcount.Valid {
		r.count = int(vcount.Int64)
	}
	if vtop.Valid {
		r.top = vtop.String
	}
	if vms.Valid {
		r.ms = vms.Float64
	}
	if vtotal.Valid {
		r.totalMs = vtotal.Float64
	}
	if verr.Valid {
		r.err = verr.String
	}
	if vat.Valid {
		r.at = vat.Int64
	}
	return &r, rs.Err()
}

// dbDeleteVisionResults purges every per-snapshot outcome of a detection.
func dbDeleteVisionResults(store *DB, id string) error {
	if store == nil {
		return fmt.Errorf("nil store")
	}
	_, e := store.db.Exec("DELETE FROM vision_results WHERE detection_id=?", id)
	return e
}

// dbSetVision persists a successful YOLO analysis for one snapshot file
// (snapIndex 0 = primary) and refreshes the BEST-result summary.
// snapPath records which file the boxes belong to (for ?boxes=1).
func dbSetVision(store *DB, id string, res *visionResult, snapPath string, snapIndex int, at int64) error {
	return dbUpsertVisionResult(store, id, snapIndex, snapPath, res, "", at)
}

// dbSetVisionSnapError records a failed per-snapshot analysis and refreshes
// the summary (which keeps showing the best successful result, if any).
func dbSetVisionSnapError(store *DB, id string, snapIndex int, snapPath string, msg string) error {
	return dbUpsertVisionResult(store, id, snapIndex, snapPath, nil, msg, time.Now().Unix())
}

// dbCountOne returns a single COUNT(*) query result (0 on nil store/error).
func dbCountOne(store *DB, q string, args ...any) int64 {
	if store == nil || store.db == nil {
		return 0
	}
	var n int64
	if err := store.db.QueryRow(q, args...).Scan(&n); err != nil {
		return 0
	}
	return n
}

// dbStatsMap returns lightweight table counts for status/diagnostics:
// detections, events (onvif_events), vision_analyzed (vision_count>0),
// s3_pending (expected but not uploaded), locked.
func dbStatsMap(db *DB) map[string]int64 {
	var out = map[string]int64{
		"detections":      0,
		"events":          0,
		"vision_analyzed": 0,
		"s3_pending":      0,
		"locked":          0,
	}
	if db == nil || db.db == nil {
		return out
	}
	out["detections"] = dbCountOne(db, "SELECT COUNT(*) FROM detections")
	out["events"] = dbCountOne(db, "SELECT COUNT(*) FROM onvif_events")
	out["vision_analyzed"] = dbCountOne(db, "SELECT COUNT(*) FROM detections WHERE vision_count > 0")
	out["s3_pending"] = dbCountOne(db, "SELECT COUNT(*) FROM detections WHERE uploaded=0 AND s3_expected=1")
	out["locked"] = dbCountOne(db, "SELECT COUNT(*) FROM detections WHERE locked != 0")
	return out
}

// dbBackupJSON returns a small JSON snapshot of DB counts + timestamp for
// status/diagnostics. Counts only, no secrets or media paths.
func dbBackupJSON(db *DB) string {
	var m = dbStatsMap(db)
	var doc = map[string]any{
		"timestamp":       time.Now().Unix(),
		"detections":      m["detections"],
		"events":          m["events"],
		"vision_analyzed": m["vision_analyzed"],
		"s3_pending":      m["s3_pending"],
		"locked":          m["locked"],
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// dbAddVisionOverride records that a vision label is excluded for one
// detection snapshot (idempotent).
func dbAddVisionOverride(db *DB, detID string, snapIndex int, label string) error {
	if db == nil || db.db == nil {
		return fmt.Errorf("nil store")
	}
	label = strings.TrimSpace(label)
	if detID == "" || label == "" {
		return fmt.Errorf("missing detection/label")
	}
	_, e := db.db.Exec("INSERT OR IGNORE INTO vision_overrides(detection_id, snap_index, label, created_at) VALUES(?,?,?,?)",
		detID, snapIndex, label, time.Now().Unix())
	return e
}

// dbListVisionOverrides returns excluded labels per snapshot index for a
// detection (empty map when none). Callers build the per-snap excluded set
// with excluded := map[string]bool{} + lower-cased labels from out[snapIndex].
func dbListVisionOverrides(db *DB, detID string) (map[int][]string, error) {
	var out = map[int][]string{}
	if db == nil || db.db == nil || detID == "" {
		return out, nil
	}
	rs, e := db.db.Query("SELECT snap_index, label FROM vision_overrides WHERE detection_id=? ORDER BY snap_index ASC", detID)
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	for rs.Next() {
		var idx int
		var label string
		if serr := rs.Scan(&idx, &label); serr != nil {
			return nil, serr
		}
		out[idx] = append(out[idx], label)
	}
	if rerr := rs.Err(); rerr != nil {
		return nil, rerr
	}
	return out, nil
}
