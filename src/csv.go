package main

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---------------- csv export: helpers --------------------

// csvEscape quotes one CSV field per RFC 4180 and neutralizes spreadsheet
// formula injection: a cell starting with =, +, - or @ (or a leading tab/CR)
// is evaluated as a formula by Excel/LibreOffice, so it is prefixed with a
// single quote - the standard mitigation.
func csvEscape(s string) string {
	if s != "" {
		switch s[0] {
		case '=', '+', '-', '@', '\t', '\r':
			s = "'" + s
		}
	}
	if !strings.ContainsAny(s, "\",\n\r") {
		return s
	}
	return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\""
}

func csvRow(fields []string) string {
	var esc = make([]string, len(fields))
	for i, f := range fields {
		esc[i] = csvEscape(f)
	}
	return strings.Join(esc, ",") + "\n"
}

func csvFileName(prefix string) string {
	return prefix + "-" + time.Now().Local().Format("20060102-150405") + ".csv"
}

func csvLocalTime(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).Local().Format("2006-01-02 15:04:05")
}

// serveDetectionsCSV exports the filtered detections table (no LIMIT
// truncation) as CSV. Same filters as the JSON endpoint. Rows are fetched in
// short 500-row cursors (never one cursor across the whole HTTP response: the
// DB pool is a single connection, so a long-lived cursor would starve writers)
// and at most one batch is held in RAM. Write errors and client disconnects
// abort the export instead of pinning the DB for a dead client.
func serveDetectionsCSV(app *App, w http.ResponseWriter, r *http.Request) {
	var cam, ty, from, to = parseListFilter(r)
	// The UI exports the current recording query unchanged, so the vision
	// filters must be honoured too (else the CSV contains rows the user
	// filtered out). Kept in the same order as listDetectionsFiltered.
	var qv = r.URL.Query()
	var label = strings.TrimSpace(qv.Get("label"))
	var hasVision = qv.Get("has_vision") == "1" || strings.EqualFold(qv.Get("has_vision"), "true")
	var q = "SELECT id, camera_id, name, det_type, confidence, started_at, ended_at, snapshot_path, clip_path, locked, uploaded FROM detections"
	var cond []string
	if from > 0 {
		cond = append(cond, "started_at >= ?")
	}
	if to > 0 {
		cond = append(cond, "started_at <= ?")
	}
	if cam != "" {
		cond = append(cond, "(camera_id = ? OR name = ?)")
	}
	if ty != "" {
		cond = append(cond, "det_type = ?")
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
	q += " ORDER BY started_at DESC"
	var args []any
	if from > 0 {
		args = append(args, from)
	}
	if to > 0 {
		args = append(args, to)
	}
	if cam != "" {
		args = append(args, cam, cam)
	}
	if ty != "" {
		args = append(args, ty)
	}
	if label != "" {
		var like = "%" + likeEscape(label) + "%"
		args = append(args, like, like)
	}
	type detRow struct {
		id, cameraID, camName, detType, snap, clip string
		conf                                       float64
		startedAt, endedAt                         int64
		locked, uploaded                           int64
	}
	var header = []string{"id", "camera_id", "camera_name", "type", "confidence", "started_at_unix", "started_at_local", "ended_at_unix", "ended_at_local", "duration_s", "size_bytes", "locked", "uploaded", "snapshot", "clip"}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+csvFileName("recordings")+"\"")
	w.WriteHeader(200)
	if _, err := w.Write([]byte("\xEF\xBB\xBF")); err != nil {
		return
	}
	if _, err := w.Write([]byte(csvRow(header))); err != nil {
		return
	}
	var fl http.Flusher
	fl, _ = w.(http.Flusher)
	const batch = 500
	for offset := 0; ; offset += batch {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		var bargs = append(append([]any{}, args...), batch, offset)
		rs, err := app.db.db.Query(q+" LIMIT ? OFFSET ?", bargs...)
		if err != nil {
			// Headers already sent: stop the export; the truncated CSV is
			// the visible signal (a 500 here would be a second header write).
			return
		}
		var rows []detRow
		for rs.Next() {
			var dr detRow
			if serr := rs.Scan(&dr.id, &dr.cameraID, &dr.camName, &dr.detType, &dr.conf, &dr.startedAt, &dr.endedAt, &dr.snap, &dr.clip, &dr.locked, &dr.uploaded); serr != nil {
				rs.Close()
				return
			}
			rows = append(rows, dr)
		}
		rs.Close()
		if rerr := rs.Err(); rerr != nil {
			// Iteration failed: stop rather than emit a truncated CSV that
			// looks complete.
			return
		}
		if len(rows) == 0 {
			return
		}
		for _, dr := range rows {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			var duration = dr.endedAt - dr.startedAt
			if duration < 0 {
				duration = 0
			}
			// detectionSize stats files per row (one stat per file); keep the
			// lightweight path: only stat when serving CSV, no vision payload.
			var totalSize = detectionSize(&Detection{id: dr.id, snapshotPath: dr.snap, clipPath: dr.clip})
			if _, err := w.Write([]byte(csvRow([]string{
				dr.id,
				dr.cameraID,
				dr.camName,
				dr.detType,
				strconv.FormatFloat(dr.conf, 'f', 2, 64),
				strconv.FormatInt(dr.startedAt, 10),
				csvLocalTime(dr.startedAt),
				strconv.FormatInt(dr.endedAt, 10),
				csvLocalTime(dr.endedAt),
				strconv.FormatInt(duration, 10),
				strconv.FormatInt(totalSize, 10),
				strconv.FormatInt(dr.locked, 10),
				strconv.FormatInt(dr.uploaded, 10),
				dr.snap,
				dr.clip,
			}))); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if len(rows) < batch {
			return
		}
	}
}

// serveOnvifEventsCSV exports the filtered raw ONVIF events table as CSV,
// fetched in short 500-row cursors (see serveDetectionsCSV for why) with
// write-error and client-disconnect aborts.
func serveOnvifEventsCSV(app *App, w http.ResponseWriter, r *http.Request) {
	var cam, ty, from, to = parseListFilter(r)
	var q = "SELECT id, camera_id, camera_name, det_type, created_at, detection_id FROM onvif_events"
	var cond []string
	if from > 0 {
		cond = append(cond, "created_at >= ?")
	}
	if to > 0 {
		cond = append(cond, "created_at <= ?")
	}
	if cam != "" {
		cond = append(cond, "(camera_id = ? OR camera_name = ?)")
	}
	if ty != "" {
		cond = append(cond, "det_type = ?")
	}
	if len(cond) > 0 {
		q += " WHERE " + strings.Join(cond, " AND ")
	}
	q += " ORDER BY created_at DESC"
	var args []any
	if from > 0 {
		args = append(args, from)
	}
	if to > 0 {
		args = append(args, to)
	}
	if cam != "" {
		args = append(args, cam, cam)
	}
	if ty != "" {
		args = append(args, ty)
	}
	type evRow struct {
		id, cameraID, camName, detType, detStr string
		createdAt                              int64
	}
	var header = []string{"id", "camera_id", "camera_name", "type", "created_at_unix", "created_at_local", "detection_id"}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+csvFileName("events")+"\"")
	w.WriteHeader(200)
	if _, err := w.Write([]byte("\xEF\xBB\xBF")); err != nil {
		return
	}
	if _, err := w.Write([]byte(csvRow(header))); err != nil {
		return
	}
	var fl http.Flusher
	fl, _ = w.(http.Flusher)
	const batch = 500
	for offset := 0; ; offset += batch {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		var bargs = append(append([]any{}, args...), batch, offset)
		rs, err := app.db.db.Query(q+" LIMIT ? OFFSET ?", bargs...)
		if err != nil {
			return
		}
		var rows []evRow
		for rs.Next() {
			var er evRow
			var detID sql.NullString
			if serr := rs.Scan(&er.id, &er.cameraID, &er.camName, &er.detType, &er.createdAt, &detID); serr != nil {
				rs.Close()
				return
			}
			if detID.Valid {
				er.detStr = detID.String
			}
			rows = append(rows, er)
		}
		rs.Close()
		if rerr := rs.Err(); rerr != nil {
			// Iteration failed: stop rather than emit a truncated CSV that
			// looks complete.
			return
		}
		if len(rows) == 0 {
			return
		}
		for _, er := range rows {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			if _, err := w.Write([]byte(csvRow([]string{
				er.id,
				er.cameraID,
				er.camName,
				er.detType,
				strconv.FormatInt(er.createdAt, 10),
				csvLocalTime(er.createdAt),
				er.detStr,
			}))); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if len(rows) < batch {
			return
		}
	}
}
