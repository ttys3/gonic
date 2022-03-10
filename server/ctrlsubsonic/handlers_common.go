package ctrlsubsonic

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"time"
	"unicode"

	"github.com/jinzhu/gorm"

	"go.senan.xyz/gonic/db"
	"go.senan.xyz/gonic/jukebox"
	"go.senan.xyz/gonic/multierr"
	"go.senan.xyz/gonic/scanner"
	"go.senan.xyz/gonic/server/ctrlsubsonic/params"
	"go.senan.xyz/gonic/server/ctrlsubsonic/spec"
	"go.senan.xyz/gonic/server/ctrlsubsonic/specid"
)

func lowerUDecOrHash(in string) string {
	lower := unicode.ToLower(rune(in[0]))
	if !unicode.IsLetter(lower) {
		return "#"
	}
	return string(lower)
}

func (c *Controller) ServeGetLicence(r *http.Request) *spec.Response {
	sub := spec.NewResponse()
	sub.Licence = &spec.Licence{
		Valid: true,
	}
	return sub
}

func (c *Controller) ServePing(r *http.Request) *spec.Response {
	return spec.NewResponse()
}

func (c *Controller) ServeScrobble(r *http.Request) *spec.Response {
	user := r.Context().Value(CtxUser).(*db.User)
	params := r.Context().Value(CtxParams).(params.Params)

	id, err := params.GetID("id")
	if err != nil || id.Type != specid.Track {
		return spec.NewError(10, "please provide a track `id` track parameter")
	}

	track := &db.Track{}
	if err := c.DB.Preload("Album").Preload("Artist").First(track, id.Value).Error; err != nil {
		return spec.NewError(0, "error finding track: %v", err)
	}

	optStamp := params.GetOrTime("time", time.Now())
	optSubmission := params.GetOrBool("submission", true)

	if err := streamUpdateStats(c.DB, user.ID, track.Album.ID, optStamp); err != nil {
		return spec.NewError(0, "error updating stats: %v", err)
	}

	var scrobbleErrs multierr.Err
	for _, scrobbler := range c.Scrobblers {
		if err := scrobbler.Scrobble(user, track, optStamp, optSubmission); err != nil {
			scrobbleErrs.Add(err)
		}
	}
	if scrobbleErrs.Len() > 0 {
		return spec.NewError(0, "error when submitting: %s", scrobbleErrs.Error())
	}

	return spec.NewResponse()
}

func (c *Controller) ServeGetMusicFolders(r *http.Request) *spec.Response {
	sub := spec.NewResponse()
	sub.MusicFolders = &spec.MusicFolders{}
	sub.MusicFolders.List = make([]*spec.MusicFolder, len(c.MusicPaths))
	for i, path := range c.MusicPaths {
		sub.MusicFolders.List[i] = &spec.MusicFolder{ID: i, Name: filepath.Base(path)}
	}
	return sub
}

func (c *Controller) ServeStartScan(r *http.Request) *spec.Response {
	go func() {
		if _, err := c.Scanner.ScanAndClean(scanner.ScanOptions{}); err != nil {
			log.Printf("error while scanning: %v\n", err)
		}
	}()
	return c.ServeGetScanStatus(r)
}

func (c *Controller) ServeGetScanStatus(r *http.Request) *spec.Response {
	var trackCount int
	if err := c.DB.Model(db.Track{}).Count(&trackCount).Error; err != nil {
		return spec.NewError(0, "error finding track count: %v", err)
	}

	sub := spec.NewResponse()
	sub.ScanStatus = &spec.ScanStatus{
		Scanning: c.Scanner.IsScanning(),
		Count:    trackCount,
	}
	return sub
}

func (c *Controller) ServeGetUser(r *http.Request) *spec.Response {
	user := r.Context().Value(CtxUser).(*db.User)
	hasLastFM := user.LastFMSession != ""
	hasListenBrainz := user.ListenBrainzToken != ""

	sub := spec.NewResponse()
	sub.User = &spec.User{
		Username:          user.Name,
		AdminRole:         user.IsAdmin,
		JukeboxRole:       c.Jukebox != nil,
		PodcastRole:       c.Podcasts != nil,
		ScrobblingEnabled: hasLastFM || hasListenBrainz,
		Folder:            []int{1},
	}
	return sub
}

func (c *Controller) ServeNotFound(r *http.Request) *spec.Response {
	return spec.NewError(70, "view not found")
}

func (c *Controller) ServeGetPlayQueue(r *http.Request) *spec.Response {
	user := r.Context().Value(CtxUser).(*db.User)
	queue := db.PlayQueue{}
	err := c.DB.
		Where("user_id=?", user.ID).
		Find(&queue).
		Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return spec.NewResponse()
	}
	sub := spec.NewResponse()
	sub.PlayQueue = &spec.PlayQueue{}
	sub.PlayQueue.Username = user.Name
	sub.PlayQueue.Position = queue.Position
	sub.PlayQueue.Current = queue.CurrentSID()
	sub.PlayQueue.Changed = queue.UpdatedAt
	sub.PlayQueue.ChangedBy = queue.ChangedBy
	trackIDs := queue.GetItems()
	sub.PlayQueue.List = make([]*spec.TrackChild, len(trackIDs))
	for i, id := range trackIDs {
		track := db.Track{}
		c.DB.
			Where("id=?", id).
			Preload("Album").
			Find(&track)
		sub.PlayQueue.List[i] = spec.NewTCTrackByFolder(&track, track.Album)
	}
	return sub
}

func (c *Controller) ServeSavePlayQueue(r *http.Request) *spec.Response {
	params := r.Context().Value(CtxParams).(params.Params)
	tracks, err := params.GetIDList("id")
	if err != nil {
		return spec.NewError(10, "please provide some `id` parameters")
	}
	// TODO: support other play queue entries other than tracks
	trackIDs := make([]int, 0, len(tracks))
	for _, id := range tracks {
		if id.Type == specid.Track {
			trackIDs = append(trackIDs, id.Value)
		}
	}
	user := r.Context().Value(CtxUser).(*db.User)
	queue := &db.PlayQueue{UserID: user.ID}
	c.DB.Where(queue).First(queue)
	queue.Current = params.GetOrID("current", specid.ID{}).Value
	queue.Position = params.GetOrInt("position", 0)
	queue.ChangedBy = params.GetOr("c", "") // must exist, middleware checks
	queue.SetItems(trackIDs)
	c.DB.Save(queue)
	return spec.NewResponse()
}

func (c *Controller) ServeGetSong(r *http.Request) *spec.Response {
	params := r.Context().Value(CtxParams).(params.Params)
	id, err := params.GetID("id")
	if err != nil {
		return spec.NewError(10, "provide an `id` parameter")
	}
	track := &db.Track{}
	err = c.DB.
		Where("id=?", id.Value).
		Preload("Album").
		Preload("Album.TagArtist").
		First(track).
		Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return spec.NewError(10, "couldn't find a track with that id")
	}
	sub := spec.NewResponse()
	sub.Track = spec.NewTrackByTags(track, track.Album)
	return sub
}

func (c *Controller) ServeGetRandomSongs(r *http.Request) *spec.Response {
	params := r.Context().Value(CtxParams).(params.Params)
	var tracks []*db.Track
	q := c.DB.DB.
		Limit(params.GetOrInt("size", 10)).
		Preload("Album").
		Preload("Album.TagArtist").
		Joins("JOIN albums ON tracks.album_id=albums.id").
		Order(gorm.Expr("random()"))
	if year, err := params.GetInt("fromYear"); err == nil {
		q = q.Where("albums.tag_year >= ?", year)
	}
	if year, err := params.GetInt("toYear"); err == nil {
		q = q.Where("albums.tag_year <= ?", year)
	}
	if genre, err := params.Get("genre"); err == nil {
		q = q.Joins("JOIN track_genres ON track_genres.track_id=tracks.id")
		q = q.Joins("JOIN genres ON genres.id=track_genres.genre_id AND genres.name=?", genre)
	}
	if m := c.getMusicFolder(params); m != "" {
		q = q.Where("albums.root_dir=?", m)
	}
	if err := q.Find(&tracks).Error; err != nil {
		return spec.NewError(10, "get random songs: %v", err)
	}
	sub := spec.NewResponse()
	sub.RandomTracks = &spec.RandomTracks{}
	sub.RandomTracks.List = make([]*spec.TrackChild, len(tracks))
	for i, track := range tracks {
		sub.RandomTracks.List[i] = spec.NewTrackByTags(track, track.Album)
	}
	return sub
}

func (c *Controller) ServeJukebox(r *http.Request) *spec.Response {
	params := r.Context().Value(CtxParams).(params.Params)
	createPlaylistItems := func(ids []specid.ID) ([]*jukebox.PlaylistItem, error) {
		var items []*jukebox.PlaylistItem
		for _, id := range ids {
			var track db.Track
			if err := c.DB.Preload("Album").First(&track, id.Value).Error; err != nil {
				return nil, fmt.Errorf("fetch track: %w", err)
			}
			items = append(items, jukebox.NewPlaylistItem(id.Value, track.AbsPath()))
		}
		return items, nil
	}
	getStatus := func() spec.JukeboxStatus {
		status := c.Jukebox.GetStatus()
		return spec.JukeboxStatus{
			CurrentIndex: status.CurrentIndex,
			Playing:      status.Playing,
			Gain:         status.Gain,
			Position:     status.Position,
		}
	}
	getStatusTracks := func() ([]*spec.TrackChild, error) {
		items := c.Jukebox.GetItems()
		ret := make([]*spec.TrackChild, len(items))
		for i, item := range items {
			var track db.Track
			if err := c.DB.Preload("Album").First(&track, item.ID()).Error; err != nil {
				return nil, fmt.Errorf("fetch track: %w", err)
			}
			ret[i] = spec.NewTrackByTags(&track, track.Album)
		}
		return ret, nil
	}

	switch act, _ := params.Get("action"); act {
	case "set":
		ids, err := params.GetIDList("id")
		if err != nil {
			return spec.NewError(10, "please provide a valid list of ids")
		}
		playlistItems, err := createPlaylistItems(ids)
		if err != nil {
			return spec.NewError(10, "error creating playlist items: %v", err)
		}
		c.Jukebox.SetItems(playlistItems)
	case "add":
		ids, err := params.GetIDList("id")
		if err != nil {
			return spec.NewError(10, "please provide a valid list of ids")
		}
		playlistItems, err := createPlaylistItems(ids)
		if err != nil {
			return spec.NewError(10, "error creating playlist items: %v", err)
		}
		c.Jukebox.AppendItems(playlistItems)
	case "clear":
		c.Jukebox.ClearItems()
	case "remove":
		index, err := params.GetInt("index")
		if err != nil {
			return spec.NewError(10, "please provide an id for remove actions")
		}
		c.Jukebox.RemoveItem(index)
	case "stop":
		c.Jukebox.Pause()
	case "start":
		c.Jukebox.Play()
	case "skip":
		index, err := params.GetInt("index")
		if err != nil {
			return spec.NewError(10, "please provide an index for skip actions")
		}
		offset, _ := params.GetInt("offset")
		c.Jukebox.Skip(index, offset)
	case "get":
		statusTracks, err := getStatusTracks()
		if err != nil {
			return spec.NewError(10, "error getting status tracks: %v", err)
		}
		sub := spec.NewResponse()
		sub.JukeboxPlaylist = &spec.JukeboxPlaylist{
			JukeboxStatus: getStatus(),
			List:          statusTracks,
		}
		return sub
	case "setGain":
		gain, err := params.GetFloat("gain")
		if err != nil {
			return spec.NewError(10, "please provide a valid gain param")
		}
		c.Jukebox.SetGain(gain)
	}
	// all actions except get are expected to return a status
	sub := spec.NewResponse()
	status := getStatus()
	sub.JukeboxStatus = &status
	return sub
}
