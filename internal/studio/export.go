package studio

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// exportFile is one file of a static export, by origin-relative path.
type exportFile struct {
	body      []byte
	immutable bool
}

// memOut collects a render in memory, over the stored files. A pin keeps
// its stored bytes, as putFile does: pins are write-once.
type memOut map[string]exportFile

func (m memOut) put(_ context.Context, path, _ string, body []byte, _, immutable bool) error {
	if prev, ok := m[path]; ok && prev.immutable {
		return nil
	}
	m[path] = exportFile{body: body, immutable: immutable}
	return nil
}

func (m memOut) remove(_ context.Context, path string) error {
	delete(m, path)
	return nil
}

// Export writes an origin's surface as a static deployment: a gzipped tar
// of the files a plain static host serves at the origin's base URL, plus
// its published media (§4: the whole surface is servable as static files).
//
// A static deployment advertises no Webmention endpoint (§15.1, §15.7), so
// the surface is re-rendered with mentions off: no manifest `webmention`
// key, no rel="webmention" link on the feed or permalink pages. Pins are
// the stored bytes. Directory pages ("", "f/{id}/") become index.html.
//
// Every URL inside is absolute against the origin's base URL, so the
// export works only when served there. The rendering runs in one
// read-only snapshot and writes nothing.
func (s *Studio) Export(ctx context.Context, originID int64, w io.Writer) error {
	return pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		o, err := scanOrigin(tx.QueryRow(ctx, `SELECT `+originCols+` FROM origins WHERE id = $1`, originID))
		if err != nil {
			return err
		}
		files := memOut{}
		rows, err := tx.Query(ctx, `SELECT path, body, immutable FROM surface_files WHERE origin_id = $1`, o.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var path string
			var f exportFile
			if err := rows.Scan(&path, &f.body, &f.immutable); err != nil {
				return err
			}
			files[path] = f
		}
		if err := rows.Err(); err != nil {
			return err
		}
		static := *o
		static.AcceptMentions = false
		if err := s.renderAllTo(ctx, tx, files, &static); err != nil {
			return err
		}

		at := time.Now().UTC().Truncate(time.Second)
		zw := gzip.NewWriter(w)
		tw := tar.NewWriter(zw)
		paths := make([]string, 0, len(files))
		for p := range files {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			name := p
			if name == "" || strings.HasSuffix(name, "/") {
				name += "index.html"
			}
			if err := writeTarFile(tw, name, files[p].body, at); err != nil {
				return err
			}
		}

		// Media as the surface serves it: only what a published version
		// references (PublicMedia).
		mrows, err := tx.Query(ctx, `SELECT m.path, m.bytes, m.created_at FROM media m
			WHERE m.origin_id = $1
			AND EXISTS (SELECT 1 FROM version_media vm WHERE vm.origin_id = m.origin_id AND vm.path = m.path)
			ORDER BY m.path`, o.ID)
		if err != nil {
			return err
		}
		defer mrows.Close()
		for mrows.Next() {
			var path string
			var body []byte
			var created time.Time
			if err := mrows.Scan(&path, &body, &created); err != nil {
				return err
			}
			if err := writeTarFile(tw, path, body, created); err != nil {
				return err
			}
		}
		if err := mrows.Err(); err != nil {
			return err
		}
		if err := tw.Close(); err != nil {
			return err
		}
		return zw.Close()
	})
}

func writeTarFile(tw *tar.Writer, name string, body []byte, at time.Time) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: at, Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
		return err
	}
	_, err := tw.Write(body)
	return err
}
