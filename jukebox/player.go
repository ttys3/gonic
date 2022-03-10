package jukebox

import (
	"fmt"
	"io"

	"github.com/hajimehoshi/oto/v2"
)

type PlayerFunc func(io.Reader) (Player, error)

type Player interface {
	Pause()
	Play()
	IsPlaying() bool
	Reset()
	Volume() float64
	SetVolume(volume float64)
	UnplayedBufferSize() int
	Close() error
}

const (
	SampleRate    = 48_000
	BitsPerSample = 16
	Channels      = 2

	BitRate = SampleRate * BitsPerSample * Channels // 1536000 b/s PCM
)

func OtoPlayer(r io.Reader) (Player, error) {
	otoc, wait, err := oto.NewContext(SampleRate, BitsPerSample/8, Channels)
	if err != nil {
		return nil, fmt.Errorf("create oto context: %w", err)
	}
	<-wait

	return otoc.NewPlayer(r), nil
}
