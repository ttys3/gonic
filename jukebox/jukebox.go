// author: AlexKraak (https://github.com/alexkraak/)
// author: sentriz (https://github.com/sentriz/)

package jukebox

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"go.senan.xyz/gonic/countrw"
	"go.senan.xyz/gonic/transcode"
)

type Jukebox struct {
	transcoder transcode.Transcoder
	pcmw       io.Writer
	pcmr       *countrw.CountReader
	player     Player

	next   chan struct{}
	cancel chan struct{}
	quit   chan struct{}

	i       int
	items   []*PlaylistItem
	itemsmu sync.RWMutex
}

func New(transcoder transcode.Transcoder, pf PlayerFunc) (*Jukebox, error) {
	var j Jukebox
	j.transcoder = transcoder

	j.cancel = make(chan struct{})
	j.next = make(chan struct{})
	j.quit = make(chan struct{})

	pcmr, pcmw := io.Pipe()
	j.pcmw = pcmw
	j.pcmr = countrw.NewCountReader(pcmr)

	var err error
	j.player, err = pf(j.pcmr)
	if err != nil {
		return nil, fmt.Errorf("create player: %w", err)
	}

	return &j, nil
}

func (j *Jukebox) DecodeStream() {
	decode := func(ctx context.Context) {
		defer func() {
			j.pcmr.Reset()
		}()
		item, err := j.Current()
		if err != nil {
			j.ClearItems()
			return
		}

		profile := transcode.WithSeek(transcode.PCM16le, item.seek)
		if err := j.transcoder.Transcode(ctx, profile, item.path, j.pcmw); err != nil {
			log.Printf("decoding item: %v", err)
		}
		j.write(func() { j.i++ })
		j.next <- struct{}{}
	}

	for {
		ctx, cancel := context.WithCancel(context.Background())
		select {
		case <-j.next:
			go func() {
				decode(ctx)
				cancel()
			}()
		case <-j.cancel:
			cancel()
		case <-j.quit:
			cancel()
			break
		}
	}
}

func (j *Jukebox) Quit() {
	j.write(func() {
		j.i = 0
		j.items = []*PlaylistItem{}
		j.pcmr.Reset()
		j.player.Close()
		close(j.quit)
	})
}

func (j *Jukebox) GetItems() []*PlaylistItem {
	var items []*PlaylistItem
	j.read(func() {
		items = append(items, j.items...)
	})
	return items
}
func (j *Jukebox) SetItems(items []*PlaylistItem) {
	j.write(func() {
		j.items = items
	})
	j.next <- struct{}{}
}
func (j *Jukebox) AppendItems(items []*PlaylistItem) {
	j.write(func() {
		j.items = append(j.items, items...)
	})
}
func (j *Jukebox) RemoveItem(i int) {
	j.write(func() {
		if !inbounds(len(j.items), i) {
			return
		}
		j.items = append(j.items[:i], j.items[i+1:]...)
	})
}

func (j *Jukebox) ClearItems() {
	j.write(func() {
		j.i = 0
		j.items = []*PlaylistItem{}
		j.player.Reset()
		j.pcmr.Reset()
	})
}

func (j *Jukebox) Pause() { j.player.Pause() }
func (j *Jukebox) Play()  { j.player.Play() }

var ErrOOB = fmt.Errorf("out of bounds")

func (j *Jukebox) Current() (*PlaylistItem, error) {
	var item *PlaylistItem
	var err error
	j.read(func() {
		if !inbounds(len(j.items), j.i) {
			err = ErrOOB
			return
		}
		item = j.items[j.i]
	})
	return item, err
}

func (j *Jukebox) Skip(i int, offsetSecs int) {
	j.write(func() {
		if !inbounds(len(j.items), i) {
			return
		}
		j.i = i
		j.items[i].seek = time.Duration(offsetSecs) * time.Second
		j.player.Play()
		j.next <- struct{}{}
	})
}

func (j *Jukebox) SetGain(v float64) { j.player.SetVolume(v) }
func (j *Jukebox) GetGain() float64  { return j.player.Volume() }

type Status struct {
	CurrentIndex int
	Playing      bool
	Gain         float64
	Position     int
}

func (j *Jukebox) GetStatus() Status {
	var status Status
	j.read(func() {
		status.CurrentIndex = j.i
		status.Playing = j.player.IsPlaying()
		status.Gain = j.player.Volume()

		if !inbounds(len(j.items), j.i) {
			return
		}

		item := j.items[j.i]
		playedBytes := j.pcmr.Count() - uint64(j.player.UnplayedBufferSize())
		playedSecs := playedBytes / (BitRate / 8)
		seekedSecs := item.seek.Seconds()
		status.Position = int(playedSecs) + int(seekedSecs)
	})
	return status
}

func (j *Jukebox) read(f func()) {
	j.itemsmu.RLock()
	defer j.itemsmu.RUnlock()
	f()
}
func (j *Jukebox) write(f func()) {
	j.itemsmu.Lock()
	defer j.itemsmu.Unlock()
	f()
}

func inbounds(length, i int) bool {
	return length > 0 && i >= 0 && i < length
}

type PlaylistItem struct {
	id   int
	path string
	seek time.Duration
}

func (p *PlaylistItem) ID() int      { return p.id }
func (p *PlaylistItem) Path() string { return p.path }

func NewPlaylistItem(id int, path string) *PlaylistItem {
	return &PlaylistItem{id: id, path: path}
}
