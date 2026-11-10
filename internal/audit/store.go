// Package audit keeps the authoritative event journal independently of Loki.
package audit

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sarattha/relayna-cell/pkg/cell"
	_ "modernc.org/sqlite"
)

const MaxJournalBytes int64 = 256 << 20
const segmentBytes int64 = 8 << 20

type Store struct {
	mu          sync.Mutex
	db          *sql.DB
	dir         string
	file        *os.File
	size, total int64
	fault       error
}

func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "cell.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, dir: filepath.Join(dir, "logs")}
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS events(seq INTEGER PRIMARY KEY AUTOINCREMENT, body BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS cells(id TEXT PRIMARY KEY, body BLOB NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	files, err := os.ReadDir(s.dir)
	if err != nil {
		db.Close()
		return nil, err
	}
	for _, f := range files {
		i, e := f.Info()
		if e != nil {
			db.Close()
			return nil, e
		}
		s.total += i.Size()
	}
	return s, nil
}

func (s *Store) Emit(cellID, execID, source, kind string, data any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fault != nil {
		return s.fault
	}
	e := cell.Event{ID: ID(), Time: time.Now().UTC(), CellID: cellID, ExecutionID: execID, Source: source, Kind: kind, Data: data}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(b) > 256<<10 {
		return fmt.Errorf("event exceeds 256 KiB")
	}
	if s.total+int64(len(b))+128 > MaxJournalBytes {
		s.fault = fmt.Errorf("audit journal is full; archive records before restarting the server")
		return s.fault
	}
	tx, err := s.db.Begin()
	if err != nil {
		s.fault = err
		return err
	}
	defer tx.Rollback()
	r, err := tx.Exec("INSERT INTO events(body) VALUES(?)", b)
	if err != nil {
		s.fault = err
		return err
	}
	e.Sequence, err = r.LastInsertId()
	if err != nil {
		s.fault = err
		return err
	}
	b, err = json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE events SET body=? WHERE seq=?", b, e.Sequence); err != nil {
		s.fault = err
		return err
	}
	if err = tx.Commit(); err != nil {
		s.fault = err
		return err
	}
	// Commit first: an export failure cannot erase evidence. Replay uses SQLite.
	if s.file == nil || s.size+int64(len(b)) > segmentBytes {
		if s.file != nil {
			if err = s.file.Close(); err != nil {
				s.fault = err
				return err
			}
		}
		s.file, err = os.OpenFile(filepath.Join(s.dir, fmt.Sprintf("events-%020d.ndjson", e.Sequence)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			s.fault = err
			return err
		}
		s.size = 0
	}
	b = append(b, '\n')
	n, err := s.file.Write(b)
	s.total += int64(n)
	s.size += int64(n)
	if err == nil {
		err = s.file.Sync()
	}
	if err != nil {
		s.fault = err
	}
	return err
}

func (s *Store) Events(cellID string, after int64, limit int) ([]cell.Event, error) {
	if limit < 1 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.Query("SELECT body FROM events WHERE seq>? AND (?='' OR json_extract(body,'$.cell_id')=?) ORDER BY seq LIMIT ?", after, cellID, cellID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []cell.Event{}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		var e cell.Event
		if err = json.Unmarshal(b, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) Save(c cell.Cell) error {
	b, e := json.Marshal(c)
	if e != nil {
		return e
	}
	_, e = s.db.Exec("INSERT INTO cells(id,body) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body", c.ID, b)
	return e
}
func (s *Store) Cells() ([]cell.Cell, error) {
	rows, e := s.db.Query("SELECT body FROM cells ORDER BY id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []cell.Cell{}
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		var c cell.Cell
		if e = json.Unmarshal(b, &c); e != nil {
			return nil, e
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) Healthy() error { s.mu.Lock(); defer s.mu.Unlock(); return s.fault }
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		s.file.Close()
	}
	return s.db.Close()
}
