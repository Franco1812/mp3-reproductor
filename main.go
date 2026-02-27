package main

import (
	"errors"
	"fmt"
	"image/color"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/data/binding"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/dhowden/tag"
	"github.com/faiface/beep"
	"github.com/faiface/beep/effects"
	"github.com/faiface/beep/flac"
	"github.com/faiface/beep/mp3"
	"github.com/faiface/beep/speaker"
)

type TrackMetadata struct {
	Title  string
	Artist string
}

type AudioEngine struct {
	mu            sync.RWMutex
	stream        beep.StreamSeekCloser
	format        beep.Format
	ctrl          *beep.Ctrl
	volume        *effects.Volume
	loaded        bool
	volumePercent float64
	metadata      TrackMetadata

	finished    chan struct{}
	speakerOnce sync.Once
	speakerRate beep.SampleRate
}

func NewAudioEngine() *AudioEngine {
	return &AudioEngine{
		volumePercent: 70,
		finished:      make(chan struct{}, 1),
		speakerRate:   beep.SampleRate(44100),
	}
}

func (e *AudioEngine) Load(path string) error {
	stream, format, err := decodeAudio(path)
	if err != nil {
		return err
	}
	meta := readMetadata(path)

	e.speakerOnce.Do(func() {
		speaker.Init(e.speakerRate, e.speakerRate.N(time.Second/20)) // Buffer corto: baja latencia y uso moderado de CPU.
	})

	e.mu.Lock()
	oldStream := e.stream

	resampled := beep.Resample(4, format.SampleRate, e.speakerRate, stream)
	seq := beep.Seq(resampled, beep.Callback(func() {
		select {
		case e.finished <- struct{}{}:
		default:
		}
	}))

	e.ctrl = &beep.Ctrl{Streamer: seq, Paused: false}
	e.volume = &effects.Volume{Streamer: e.ctrl, Base: 2}
	applyVolume(e.volume, e.volumePercent)

	e.stream = stream
	e.format = format
	e.loaded = true
	e.metadata = meta
	e.mu.Unlock()

	// Clear/Play ya manejan su propia sincronización interna en beep/speaker.
	speaker.Clear()
	speaker.Play(e.volume)

	if oldStream != nil {
		_ = oldStream.Close()
	}

	return nil
}

func (e *AudioEngine) TogglePause() (bool, error) {
	e.mu.RLock()
	ctrl := e.ctrl
	loaded := e.loaded
	e.mu.RUnlock()

	if !loaded || ctrl == nil {
		return false, errors.New("no hay pista cargada")
	}

	speaker.Lock()
	ctrl.Paused = !ctrl.Paused
	paused := ctrl.Paused
	speaker.Unlock()

	return paused, nil
}

func (e *AudioEngine) Stop() error {
	e.mu.RLock()
	stream := e.stream
	ctrl := e.ctrl
	loaded := e.loaded
	e.mu.RUnlock()

	if !loaded || stream == nil || ctrl == nil {
		return errors.New("no hay pista cargada")
	}

	speaker.Lock()
	_ = stream.Seek(0)
	ctrl.Paused = true
	speaker.Unlock()
	return nil
}

func (e *AudioEngine) SetVolume(percent float64) {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}

	e.mu.Lock()
	e.volumePercent = percent
	vol := e.volume
	e.mu.Unlock()

	if vol != nil {
		speaker.Lock()
		applyVolume(vol, percent)
		speaker.Unlock()
	}
}

func (e *AudioEngine) Progress() (elapsed, total time.Duration, ratio float64) {
	e.mu.RLock()
	stream := e.stream
	format := e.format
	loaded := e.loaded
	e.mu.RUnlock()

	if !loaded || stream == nil {
		return 0, 0, 0
	}

	// speaker.Lock evita carreras con el hilo de audio al leer Position.
	speaker.Lock()
	pos := stream.Position()
	speaker.Unlock()

	elapsed = format.SampleRate.D(pos)
	return elapsed, 0, 0
}

func (e *AudioEngine) Metadata() TrackMetadata {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.metadata
}

func (e *AudioEngine) Close() {
	e.mu.Lock()
	stream := e.stream
	e.stream = nil
	e.ctrl = nil
	e.volume = nil
	e.loaded = false
	e.mu.Unlock()

	speaker.Clear()

	if stream != nil {
		_ = stream.Close()
	}
}

func decodeAudio(path string) (beep.StreamSeekCloser, beep.Format, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, beep.Format{}, err
	}

	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp3":
		s, format, err := mp3.Decode(f)
		if err != nil {
			_ = f.Close()
			return nil, beep.Format{}, err
		}
		return s, format, nil
	case ".flac":
		s, format, err := flac.Decode(f)
		if err != nil {
			_ = f.Close()
			return nil, beep.Format{}, err
		}
		return s, format, nil
	default:
		_ = f.Close()
		return nil, beep.Format{}, fmt.Errorf("formato no soportado: %s", ext)
	}
}

func readMetadata(path string) TrackMetadata {
	meta := TrackMetadata{
		Title:  strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		Artist: "Unknown Artist",
	}

	f, err := os.Open(path)
	if err != nil {
		return meta
	}
	defer f.Close()

	m, err := tag.ReadFrom(f)
	if err != nil {
		return meta
	}

	if t := strings.TrimSpace(m.Title()); t != "" {
		meta.Title = t
	}
	if a := strings.TrimSpace(m.Artist()); a != "" {
		meta.Artist = a
	}
	return meta
}

func applyVolume(v *effects.Volume, percent float64) {
	if percent <= 0 {
		v.Silent = true
		return
	}
	v.Silent = false
	lin := percent / 100.0
	v.Volume = math.Log2(lin) // Mapeo visual lineal a ganancia percibida.
}

type metalTheme struct{}

func (m *metalTheme) Color(name fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNameBackground:
		return color.NRGBA{R: 18, G: 20, B: 22, A: 255}
	case theme.ColorNameButton:
		return color.NRGBA{R: 45, G: 49, B: 52, A: 255}
	case theme.ColorNameInputBackground:
		return color.NRGBA{R: 28, G: 31, B: 34, A: 255}
	case theme.ColorNamePrimary:
		return color.NRGBA{R: 185, G: 92, B: 35, A: 255}
	case theme.ColorNameForeground:
		return color.NRGBA{R: 222, G: 222, B: 222, A: 255}
	default:
		return theme.DefaultTheme().Color(name, theme.VariantDark)
	}
}

func (m *metalTheme) Font(style fyne.TextStyle) fyne.Resource {
	return theme.DefaultTheme().Font(style)
}

func (m *metalTheme) Icon(name fyne.ThemeIconName) fyne.Resource {
	return theme.DefaultTheme().Icon(name)
}

func (m *metalTheme) Size(name fyne.ThemeSizeName) float32 {
	return theme.DefaultTheme().Size(name)
}

func main() {
	a := app.NewWithID("metal.audio.player")
	a.Settings().SetTheme(&metalTheme{})

	engine := NewAudioEngine()
	w := a.NewWindow("Metal Player")
	w.Resize(fyne.NewSize(720, 360))
	w.CenterOnScreen()

	titleBind := binding.NewString()
	artistBind := binding.NewString()
	statusBind := binding.NewString()
	timeBind := binding.NewString()
	progressBind := binding.NewFloat()

	_ = titleBind.Set("Title: -")
	_ = artistBind.Set("Artist: -")
	_ = statusBind.Set("Estado: sin pista")
	_ = timeBind.Set("00:00 / 00:00")
	_ = progressBind.Set(0)

	titleLabel := widget.NewLabelWithData(titleBind)
	artistLabel := widget.NewLabelWithData(artistBind)
	statusLabel := widget.NewLabelWithData(statusBind)
	progress := widget.NewProgressBarWithData(progressBind)
	timeLabel := widget.NewLabelWithData(timeBind)
	var loadSeq int64

	openBtn := widget.NewButton("Abrir MP3/FLAC", func() {
		fd := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
			if err != nil || rc == nil {
				return
			}
			path := normalizePathFromURI(rc.URI())
			_ = rc.Close()
			loadID := atomic.AddInt64(&loadSeq, 1)

			// Goroutine para decode + I/O: evita congelar el hilo principal de la UI.
			go func() {
				_ = statusBind.Set("Estado: cargando...")

				result := make(chan error, 1)
				go func() {
					result <- engine.Load(path)
				}()

				select {
				case err := <-result:
					if loadID != atomic.LoadInt64(&loadSeq) {
						return
					}
					if err != nil {
						_ = statusBind.Set("Error: " + err.Error())
						return
					}
					md := engine.Metadata()
					_ = titleBind.Set("Title: " + md.Title)
					_ = artistBind.Set("Artist: " + md.Artist)
					_ = statusBind.Set("Estado: reproduciendo")
				case <-time.After(12 * time.Second):
					if loadID != atomic.LoadInt64(&loadSeq) {
						return
					}
					_ = statusBind.Set("Error: timeout cargando audio")
				}
			}()
		}, w)

		fd.SetFilter(storage.NewExtensionFileFilter([]string{".mp3", ".flac"}))
		fd.Show()
	})

	playPauseBtn := widget.NewButton("Play/Pause", func() {
		paused, err := engine.TogglePause()
		if err != nil {
			_ = statusBind.Set("Error: " + err.Error())
			return
		}
		if paused {
			_ = statusBind.Set("Estado: pausado")
		} else {
			_ = statusBind.Set("Estado: reproduciendo")
		}
	})

	stopBtn := widget.NewButton("Stop", func() {
		if err := engine.Stop(); err != nil {
			_ = statusBind.Set("Error: " + err.Error())
			return
		}
		_ = statusBind.Set("Estado: detenido")
		_ = progressBind.Set(0)
		_ = timeBind.Set("00:00 / 00:00")
	})

	vol := widget.NewSlider(0, 100)
	vol.Step = 1
	vol.Value = 70
	vol.OnChanged = func(v float64) { engine.SetVolume(v) }

	controls := container.NewGridWithColumns(3, openBtn, playPauseBtn, stopBtn)
	volumeRow := container.NewBorder(nil, nil, widget.NewLabel("Vol"), widget.NewLabel("100"), vol)
	metaBox := container.NewVBox(titleLabel, artistLabel, statusLabel)
	content := container.NewVBox(metaBox, progress, timeLabel, controls, volumeRow)
	w.SetContent(container.NewPadded(content))

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond) // Refresco periódico: progreso fluido sin sobrecargar CPU.
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-engine.finished:
				_ = statusBind.Set("Estado: finalizado")
			case <-ticker.C:
				elapsed, total, ratio := engine.Progress()
				_ = progressBind.Set(ratio)
				_ = timeBind.Set(fmt.Sprintf("%s / %s", fmtDur(elapsed), fmtDur(total)))
			}
		}
	}()

	w.SetOnClosed(func() {
		close(done)
		engine.Close()
	})

	w.ShowAndRun()
}

func normalizePathFromURI(u fyne.URI) string {
	p := u.Path()
	if decoded, err := url.PathUnescape(p); err == nil {
		p = decoded
	}
	if runtime.GOOS == "windows" && len(p) > 2 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return filepath.FromSlash(p)
}

func fmtDur(d time.Duration) string {
	if d <= 0 {
		return "00:00"
	}
	total := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d", total/60, total%60)
}
