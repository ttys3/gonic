package scanner

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jinzhu/gorm"
	"github.com/karrick/godirwalk"
	"github.com/rainycape/unidecode"

	"go.senan.xyz/gonic/server/db"
	"go.senan.xyz/gonic/server/mime"
	"go.senan.xyz/gonic/server/scanner/stack"
	"go.senan.xyz/gonic/server/scanner/tags"
)

var (
	ErrAlreadyScanning = errors.New("already scanning")
	ErrStatingItem     = errors.New("could not stat item")
	ErrReadingTags     = errors.New("could not read tags")
)

func durSince(t time.Time) time.Duration {
	return time.Since(t).Truncate(10 * time.Microsecond)
}

// decoded converts a string to it's latin equivalent.
// it will be used by the model's *UDec fields, and is only set if it
// differs from the original. the fields are used for searching.
func decoded(in string) string {
	if u := unidecode.Unidecode(in); u != in {
		return u
	}
	return ""
}

// isScanning acts as an atomic boolean semaphore. we don't
// want to have more than one scan going on at a time
var isScanning int32 //nolint:gochecknoglobals

func IsScanning() bool {
	return atomic.LoadInt32(&isScanning) == 1
}

func SetScanning() func() {
	atomic.StoreInt32(&isScanning, 1)
	return func() {
		atomic.StoreInt32(&isScanning, 0)
	}
}

type Scanner struct {
	db         *db.DB
	musicPath  string
	isFull     bool
	genreSplit string
	// these two are for the transaction we do for every folder.
	// the boolean is there so we dont begin or commit multiple
	// times in the handle folder or post children callback
	trTx     *db.DB
	trTxOpen bool
	// these two are for keeping state between noted in the tree.
	// eg. keep track of a parents folder or the path to a cover
	// we just saw that we need to commit in the post children
	// callback
	curFolders *stack.Stack
	curCover   string
	// then the rest are for stats and cleanup at the very end
	seenTracks    map[int]struct{} // set of p keys
	seenFolders   map[int]struct{} // set of p keys
	seenTracksNew int              // n tracks not seen before
}

func New(musicPath string, db *db.DB, genreSplit string) *Scanner {
	return &Scanner{
		db:         db,
		musicPath:  musicPath,
		genreSplit: genreSplit,
	}
}

// ## begin clean funcs
// ## begin clean funcs
// ## begin clean funcs

func (s *Scanner) cleanTracks() error {
	start := time.Now()
	var previous []int
	var missing []int64
	err := s.db.
		Model(&db.Track{}).
		Pluck("id", &previous).
		Error
	if err != nil {
		return fmt.Errorf("plucking ids: %w", err)
	}
	for _, prev := range previous {
		if _, ok := s.seenTracks[prev]; !ok {
			missing = append(missing, int64(prev))
		}
	}
	err = s.db.TransactionChunked(missing, func(tx *gorm.DB, chunk []int64) error {
		return tx.Where(chunk).Delete(&db.Track{}).Error
	})
	if err != nil {
		return err
	}
	log.Printf("finished clean tracks in %s, %d removed", durSince(start), len(missing))
	return nil
}

func (s *Scanner) cleanFolders() error {
	start := time.Now()
	var previous []int
	var missing []int64
	err := s.db.
		Model(&db.Album{}).
		Pluck("id", &previous).
		Error
	if err != nil {
		return fmt.Errorf("plucking ids: %w", err)
	}
	for _, prev := range previous {
		if _, ok := s.seenFolders[prev]; !ok {
			missing = append(missing, int64(prev))
		}
	}
	err = s.db.TransactionChunked(missing, func(tx *gorm.DB, chunk []int64) error {
		return tx.Where(chunk).Delete(&db.Album{}).Error
	})
	if err != nil {
		return err
	}
	log.Printf("finished clean folders in %s, %d removed", durSince(start), len(missing))
	return nil
}

func (s *Scanner) cleanArtists() error {
	start := time.Now()
	sub := s.db.
		Select("artists.id").
		Model(&db.Artist{}).
		Joins("LEFT JOIN albums ON albums.tag_artist_id=artists.id").
		Where("albums.id IS NULL").
		SubQuery()
	q := s.db.
		Where("artists.id IN ?", sub).
		Delete(&db.Artist{})
	if err := q.Error; err != nil {
		return err
	}
	log.Printf("finished clean artists in %s, %d removed", durSince(start), q.RowsAffected)
	return nil
}

func (s *Scanner) cleanGenres() error {
	start := time.Now()
	subTrack := s.db.
		Select("genres.id").
		Model(&db.Genre{}).
		Joins("LEFT JOIN track_genres ON track_genres.genre_id=genres.id").
		Where("track_genres.genre_id IS NULL").
		SubQuery()
	subAlbum := s.db.
		Select("genres.id").
		Model(&db.Genre{}).
		Joins("LEFT JOIN album_genres ON album_genres.genre_id=genres.id").
		Where("album_genres.genre_id IS NULL").
		SubQuery()
	q := s.db.
		Where("genres.id IN ?", subTrack).
		Or("genres.id IN ?", subAlbum).
		Delete(&db.Genre{})
	log.Printf("finished clean genres in %s, %d removed", durSince(start), q.RowsAffected)
	return nil
}

// ## begin entries
// ## begin entries
// ## begin entries

type ScanOptions struct {
	IsFull bool
	// TODO https://github.com/sentriz/gonic/issues/64
	Path string
}

func (s *Scanner) Start(opts ScanOptions) error {
	if IsScanning() {
		return ErrAlreadyScanning
	}
	unSet := SetScanning()
	defer unSet()

	// reset state vars for the new scan
	s.isFull = opts.IsFull
	s.seenTracks = map[int]struct{}{}
	s.seenFolders = map[int]struct{}{}
	s.curFolders = &stack.Stack{}
	s.seenTracksNew = 0

	// begin walking
	log.Println("starting scan")
	var errCount int
	start := time.Now()
	err := godirwalk.Walk(s.musicPath, &godirwalk.Options{
		Callback:             s.callbackItem,
		PostChildrenCallback: s.callbackPost,
		Unsorted:             true,
		FollowSymbolicLinks:  true,
		ErrorCallback: func(path string, err error) godirwalk.ErrorAction {
			log.Printf("error processing `%s`: %v", path, err)
			errCount++
			return godirwalk.SkipNode
		},
	})
	if err != nil {
		return fmt.Errorf("walking filesystem: %w", err)
	}
	log.Printf("finished scan in %s, +%d/%d tracks (%d err)\n",
		durSince(start),
		s.seenTracksNew,
		len(s.seenTracks),
		errCount,
	)

	if err := s.cleanTracks(); err != nil {
		return fmt.Errorf("clean tracks: %w", err)
	}
	if err := s.cleanFolders(); err != nil {
		return fmt.Errorf("clean folders: %w", err)
	}
	if err := s.cleanArtists(); err != nil {
		return fmt.Errorf("clean artists: %w", err)
	}
	if err := s.cleanGenres(); err != nil {
		return fmt.Errorf("clean genres: %w", err)
	}

	// finish up
	strNow := strconv.FormatInt(time.Now().Unix(), 10)
	s.db.SetSetting("last_scan_time", strNow)
	return nil
}

// items are passed to the handle*() functions
type item struct {
	fullPath  string
	relPath   string
	directory string
	filename  string
	stat      os.FileInfo
}

func isCover(filename string) bool {
	filename = strings.ToLower(filename)
	known := map[string]struct{}{
		"cover.png":     {},
		"cover.jpg":     {},
		"cover.jpeg":    {},
		"folder.png":    {},
		"folder.jpg":    {},
		"folder.jpeg":   {},
		"album.png":     {},
		"album.jpg":     {},
		"album.jpeg":    {},
		"albumart.png":  {},
		"albumart.jpg":  {},
		"albumart.jpeg": {},
		"front.png":     {},
		"front.jpg":     {},
		"front.jpeg":    {},
	}
	_, ok := known[filename]
	return ok
}

// ## begin callbacks
// ## begin callbacks
// ## begin callbacks

func (s *Scanner) callbackItem(fullPath string, info *godirwalk.Dirent) error {
	stat, err := os.Stat(fullPath)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStatingItem, err)
	}
	relPath, err := filepath.Rel(s.musicPath, fullPath)
	if err != nil {
		return fmt.Errorf("getting relative path: %w", err)
	}
	directory, filename := path.Split(relPath)
	it := &item{
		fullPath:  fullPath,
		relPath:   relPath,
		directory: directory,
		filename:  filename,
		stat:      stat,
	}
	isDir, err := info.IsDirOrSymlinkToDir()
	if err != nil {
		return fmt.Errorf("stating link to dir: %w", err)
	}
	if isDir {
		return s.handleFolder(it)
	}
	if isCover(filename) {
		s.curCover = filename
		return nil
	}
	ext := path.Ext(filename)
	if ext == "" {
		return nil
	}
	if _, ok := mime.FromExtension(ext[1:]); ok {
		return s.handleTrack(it)
	}
	return nil
}

func (s *Scanner) callbackPost(fullPath string, info *godirwalk.Dirent) error {
	defer func() {
		s.curCover = ""
	}()
	if s.trTxOpen {
		s.trTx.Commit()
		s.trTxOpen = false
	}
	// begin taking the current folder off the stack and add it's
	// parent, cover that we found, etc.
	folder := s.curFolders.Pop()
	if folder.ParentID != 0 {
		return nil
	}
	folder.ParentID = s.curFolders.PeekID()
	folder.Cover = s.curCover
	if err := s.db.Save(folder).Error; err != nil {
		return fmt.Errorf("writing albums table: %w", err)
	}
	// we only log changed folders
	log.Printf("processed folder `%s`\n",
		path.Join(folder.LeftPath, folder.RightPath))
	return nil
}

// ## begin handlers
// ## begin handlers
// ## begin handlers

func (s *Scanner) itemUnchanged(statModTime, updatedInDB time.Time) bool {
	if s.isFull {
		return false
	}
	return statModTime.Before(updatedInDB)
}

func (s *Scanner) handleFolder(it *item) error {
	if s.trTxOpen {
		// a transaction still being open when we handle a folder can
		// happen if there is a folder that contains /both/ tracks and
		// sub folders
		s.trTx.Commit()
		s.trTxOpen = false
	}
	folder := &db.Album{}
	defer func() {
		// folder's id will come from early return
		// or save at the end
		s.seenFolders[folder.ID] = struct{}{}
		s.curFolders.Push(folder)
	}()
	err := s.db.
		Where(db.Album{
			LeftPath:  it.directory,
			RightPath: it.filename,
		}).
		First(folder).
		Error
	if !gorm.IsRecordNotFoundError(err) &&
		s.itemUnchanged(it.stat.ModTime(), folder.UpdatedAt) {
		// we found the record but it hasn't changed
		return nil
	}
	folder.LeftPath = it.directory
	folder.RightPath = it.filename
	folder.RightPathUDec = decoded(it.filename)
	folder.ModifiedAt = it.stat.ModTime()
	if err := s.db.Save(folder).Error; err != nil {
		return fmt.Errorf("writing albums table: %w", err)
	}
	return nil
}

func (s *Scanner) handleTrack(it *item) error {
	if !s.trTxOpen {
		s.trTx = s.db.Begin()
		s.trTxOpen = true
	}

	// set track basics
	track := &db.Track{}
	defer func() {
		// folder's id will come from early return
		// or save at the end
		s.seenTracks[track.ID] = struct{}{}
	}()
	err := s.trTx.
		Select("id, updated_at").
		Where(db.Track{
			AlbumID:  s.curFolders.PeekID(),
			Filename: it.filename,
		}).
		First(track).
		Error
	if !gorm.IsRecordNotFoundError(err) &&
		s.itemUnchanged(it.stat.ModTime(), track.UpdatedAt) {
		// we found the record but it hasn't changed
		return nil
	}
	track.Filename = it.filename
	track.FilenameUDec = decoded(it.filename)
	track.Size = int(it.stat.Size())
	track.AlbumID = s.curFolders.PeekID()
	trTags, err := tags.New(it.fullPath)
	if err != nil {
		return ErrReadingTags
	}
	track.TagTitle = trTags.Title()
	track.TagTitleUDec = decoded(trTags.Title())
	track.TagTrackArtist = trTags.Artist()
	track.TagTrackNumber = trTags.TrackNumber()
	track.TagDiscNumber = trTags.DiscNumber()
	track.TagBrainzID = trTags.BrainzID()
	track.Length = trTags.Length()   // these two should be calculated
	track.Bitrate = trTags.Bitrate() // ...from the file instead of tags

	// set album artist basics
	artistName := firstTag("Unknown Artist", trTags.AlbumArtist, trTags.Artist)
	artist := &db.Artist{}
	s.trTx.
		Where("name=?", artistName).
		Assign(db.Artist{
			Name:     artistName,
			NameUDec: decoded(artistName),
		}).
		FirstOrCreate(artist)
	track.ArtistID = artist.ID

	// set genre
	genreTag := firstTag("Unknown Genre", trTags.Genre)
	genreNames := strings.Split(genreTag, s.genreSplit)
	genreIDs := []int{}
	for _, genreName := range genreNames {
		genre := &db.Genre{}
		s.trTx.FirstOrCreate(genre, db.Genre{
			Name: genreName,
		})
		genreIDs = append(genreIDs, genre.ID)
	}

	if err := s.trTx.Save(track).Error; err != nil {
		return fmt.Errorf("writing track table: %w", err)
	}

	err = s.trTx.
		Where("track_id=?", track.ID).
		Delete(db.TrackGenre{}).
		Error
	if err != nil {
		return fmt.Errorf("delete old track genre records: %w", err)
	}
	err = s.trTx.InsertBulkLeftMany(
		"track_genres",
		[]string{"track_id", "genre_id"},
		track.ID,
		genreIDs,
	)
	if err != nil {
		return fmt.Errorf("insert bulk track genres: %w", err)
	}
	s.seenTracksNew++

	// set album if this is the first track in the folder
	folder := s.curFolders.Peek()
	if folder.TagTitle != "" {
		return nil
	}
	err = s.trTx.
		Where("album_id=?", folder.ID).
		Delete(db.AlbumGenre{}).
		Error
	if err != nil {
		return fmt.Errorf("delete old album genre records: %w", err)
	}
	err = s.trTx.InsertBulkLeftMany(
		"album_genres",
		[]string{"album_id", "genre_id"},
		folder.ID,
		genreIDs,
	)
	if err != nil {
		return fmt.Errorf("insert bulk album genres: %w", err)
	}
	folder.TagTitle = trTags.Album()
	folder.TagTitleUDec = decoded(trTags.Album())
	folder.TagBrainzID = trTags.AlbumBrainzID()
	folder.TagYear = trTags.Year()
	folder.TagArtistID = artist.ID
	return nil
}

func firstTag(fallback string, tags ...func() string) string {
	for _, f := range tags {
		if tag := f(); tag != "" {
			return tag
		}
	}
	return fallback
}
