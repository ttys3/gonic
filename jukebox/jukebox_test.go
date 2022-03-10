package jukebox_test

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/matryer/is"
	"go.senan.xyz/gonic/iout"
	"go.senan.xyz/gonic/jukebox"
	"go.senan.xyz/gonic/transcode"
)

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		os.Exit(0)
		return
	}
	os.Exit(m.Run())
}

func TestPlay(t *testing.T) {
	t.Parallel()
	j := newJukebox(t)
	is := is.New(t)

	type ch struct {
		i       int
		secs    int
		playing bool
	}

	check := func(c ch) {
		is.Helper()
		status := j.GetStatus()
		is.Equal(status.CurrentIndex, c.i)
		is.Equal(status.Position, c.secs)
		is.Equal(status.Playing, c.playing)
		is.Equal(status.Gain, 1.0)
	}

	go j.DecodeStream()

	j.SetItems([]*jukebox.PlaylistItem{
		jukebox.NewPlaylistItem(0, "testdata/10s.mp3"),
		jukebox.NewPlaylistItem(0, "testdata/10s.mp3"),
	})

	check(ch{i: 0, secs: 0})
	j.Play()
	check(ch{i: 0, secs: 0, playing: true})
	j.Pause()
	check(ch{i: 0, secs: 0, playing: false})
	j.Play()
	check(ch{i: 0, secs: 0, playing: true})

	// read the whole the first 10s track
	j.player.ReadN(secsToBytes(10))
	time.Sleep(100 * time.Millisecond) // let process exit, skip to next

	check(ch{i: 1, secs: 0, playing: true})

	// then half the second
	j.player.ReadN(secsToBytes(5))
	check(ch{i: 1, secs: 5, playing: true})

	// then the other half
	j.player.ReadN(secsToBytes(5))
	time.Sleep(100 * time.Millisecond) // let process exit, skip to next

	check(ch{i: 0, secs: 0})
}

func TestSkip(t *testing.T) {
	t.Parallel()
	j := newJukebox(t)
	is := is.New(t)

	go j.DecodeStream()

	is.NoErr(withTimeout(1*time.Second, func() {
		j.Skip(10, 10)
	}))

	is.Equal(j.GetStatus().CurrentIndex, 0) // no change, out of bounds
	is.True(!j.GetStatus().Playing)         // no change, out of bounds

	j.SetItems([]*jukebox.PlaylistItem{
		jukebox.NewPlaylistItem(0, "testdata/5s.mp3"),
		jukebox.NewPlaylistItem(0, "testdata/5s.mp3"),
		jukebox.NewPlaylistItem(0, "testdata/5s.mp3"),
	})
	j.Play()

	j.player.ReadN(secsToBytes(1))
	is.True(j.GetStatus().Playing)      // first track, read 1 sec, playing
	is.Equal(j.GetStatus().Position, 1) // first track, read 1 sec, position 1 sec

	is.NoErr(withTimeout(1*time.Second, func() {
		j.Skip(0, 2)
	}))

	j.player.ReadN(secsToBytes(1))
	is.True(j.GetStatus().Playing)      // first track, seek 2 secs, read 1 sec, playing
	is.Equal(j.GetStatus().Position, 4) // first track, seek 2 secs, read 1 sec, position 3 secs

	is.NoErr(withTimeout(1*time.Second, func() {
		j.Skip(1, 0)
	}))

	j.player.ReadN(secsToBytes(1))
	is.True(j.GetStatus().Playing)      // second track track, read 1 sec, playing
	is.Equal(j.GetStatus().Position, 3) // second track track, read 1 sec, position 3 secs
}

func TestQuit(t *testing.T) {
	t.Parallel()
	j := newJukebox(t)
	is := is.New(t)

	go j.DecodeStream()
	j.SetItems([]*jukebox.PlaylistItem{jukebox.NewPlaylistItem(0, "testdata/10s.mp3")})
	j.Play()

	// we should be able to quit even if we're in the middle of transcoding
	is.NoErr(withTimeout(1*time.Second, func() {
		j.Quit()
	}))
}

func TestPlaylist(t *testing.T) {
	j := newJukebox(t)
	is := is.New(t)

	go j.DecodeStream()

	is.Equal(len(j.GetItems()), 0)

	j.SetItems([]*jukebox.PlaylistItem{
		jukebox.NewPlaylistItem(0, "testdata/5s.mp3"),
		jukebox.NewPlaylistItem(1, "testdata/5s.mp3"),
		jukebox.NewPlaylistItem(2, "testdata/5s.mp3"),
	})
	is.Equal(len(j.GetItems()), 3)

	j.AppendItems([]*jukebox.PlaylistItem{
		jukebox.NewPlaylistItem(3, "testdata/5s.mp3"),
	})
	is.Equal(len(j.GetItems()), 4)
	is.Equal(j.GetItems()[0].ID(), 0)
	is.Equal(j.GetItems()[1].ID(), 1)
	is.Equal(j.GetItems()[2].ID(), 2)
	is.Equal(j.GetItems()[3].ID(), 3)

	j.RemoveItem(1)
	is.Equal(len(j.GetItems()), 3)
	is.Equal(j.GetItems()[0].ID(), 0)
	is.Equal(j.GetItems()[1].ID(), 2)
	is.Equal(j.GetItems()[2].ID(), 3)

	j.RemoveItem(10)
	is.Equal(len(j.GetItems()), 3)
}

func TestGain(t *testing.T) {
	j := newJukebox(t)
	is := is.New(t)

	is.Equal(j.GetStatus().Gain, 1.0)
	is.Equal(j.GetGain(), 1.0)

	j.SetGain(0)
	is.Equal(j.GetStatus().Gain, 0.0)
	is.Equal(j.GetGain(), 0.0)

	j.SetGain(0.5)
	is.Equal(j.GetStatus().Gain, 0.5)
	is.Equal(j.GetGain(), 0.5)
}

type mockJukebox struct {
	t *testing.T
	*jukebox.Jukebox
	player   *mockPlayer
	quitOnce sync.Once
}

func newJukebox(t *testing.T) *mockJukebox {
	var m *mockPlayer
	j, err := jukebox.New(
		transcode.NewFFmpegTranscoder(),
		func(r io.Reader) (jukebox.Player, error) {
			m = &mockPlayer{r: iout.NewCountReader(r), gain: 1}
			return m, nil
		},
	)
	if err != nil {
		t.Fatalf("error creating jukebox: %v", err)
	}
	mj := &mockJukebox{t: t, Jukebox: j, player: m}
	t.Cleanup(func() {
		mj.Quit()
	})
	return mj
}

func (mj *mockJukebox) Quit() {
	mj.quitOnce.Do(mj.Jukebox.Quit)
}

type mockPlayer struct {
	r       *iout.CountReader
	playing bool
	gain    float64
}

func (m *mockPlayer) Pause()                  { m.playing = false }
func (m *mockPlayer) Play()                   { m.playing = true }
func (m *mockPlayer) IsPlaying() bool         { return m.playing }
func (m *mockPlayer) Reset()                  { m.playing = false }
func (m *mockPlayer) Volume() float64         { return m.gain }
func (m *mockPlayer) SetVolume(gain float64)  { m.gain = gain }
func (m *mockPlayer) UnplayedBufferSize() int { return 0 }
func (m *mockPlayer) Close() error            { return nil }

func (m *mockPlayer) ReadN(to int) {
	var read int
	for read < to {
		n, _ := m.r.Read(make([]byte, min(to-read, 2<<14)))
		read += n
	}
}

var _ jukebox.Player = (*mockPlayer)(nil)

var ErrTimeout = fmt.Errorf("timeout")

func withTimeout(d time.Duration, f func()) error {
	done := make(chan struct{})
	go func() {
		f()
		close(done)
	}()

	select {
	case <-time.After(d):
		return ErrTimeout
	case <-done:
		return nil
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func secsToBytes(secs int) int {
	return secs * (jukebox.BitRate / 8)
}
