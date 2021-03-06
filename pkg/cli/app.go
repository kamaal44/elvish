// Package cli implements a generic interactive line editor.
package cli

import (
	"io"
	"os"
	"sync"
	"syscall"

	"src.elv.sh/pkg/cli/term"
	"src.elv.sh/pkg/cli/tk"
	"src.elv.sh/pkg/sys"
)

// App represents a CLI app.
type App interface {
	// ReadCode requests the App to read code from the terminal by running an
	// event loop. This function is not re-entrant.
	ReadCode() (string, error)

	// MutateState mutates the state of the app.
	MutateState(f func(*State))
	// CopyState returns a copy of the a state.
	CopyState() State
	// CodeArea returns the codearea widget of the app.
	CodeArea() tk.CodeArea
	// SetAddon sets the current addon to the given widget. If there is an
	// existing addon, it is closed first. If the existing addon implements
	// interface{ Close(bool) }, the Close method is called with the accept
	// argument. To close the current addon without setting a new one, call
	// SetAddon(nil, accept).
	SetAddon(w tk.Widget, accept bool)

	// CommitEOF causes the main loop to exit with EOF. If this method is called
	// when an event is being handled, the main loop will exit after the handler
	// returns.
	CommitEOF()
	// CommitCode causes the main loop to exit with the current code content. If
	// this method is called when an event is being handled, the main loop will
	// exit after the handler returns.
	CommitCode()

	// Redraw requests a redraw. It never blocks and can be called regardless of
	// whether the App is active or not.
	Redraw()
	// RedrawFull requests a full redraw. It never blocks and can be called
	// regardless of whether the App is active or not.
	RedrawFull()
	// Notify adds a note and requests a redraw.
	Notify(note string)
}

type app struct {
	loop    *loop
	reqRead chan struct{}

	TTY               TTY
	MaxHeight         func() int
	RPromptPersistent func() bool
	BeforeReadline    []func()
	AfterReadline     []func(string)
	Highlighter       Highlighter
	Prompt            Prompt
	RPrompt           Prompt
	GlobalBindings    tk.Bindings

	StateMutex sync.RWMutex
	State      State

	codeArea tk.CodeArea
}

// State represents mutable state of an App.
type State struct {
	// Notes that have been added since the last redraw.
	Notes []string
	// An addon widget. When non-nil, it is shown under the codearea widget and
	// terminal events are handled by it.
	//
	// The cursor is placed on the addon by default. If the addon widget
	// implements interface{ Focus() bool }, the Focus method is called to
	// determine that instead.
	Addon tk.Widget
}

// NewApp creates a new App from the given specification.
func NewApp(spec AppSpec) App {
	lp := newLoop()
	a := app{
		loop:              lp,
		TTY:               spec.TTY,
		MaxHeight:         spec.MaxHeight,
		RPromptPersistent: spec.RPromptPersistent,
		BeforeReadline:    spec.BeforeReadline,
		AfterReadline:     spec.AfterReadline,
		Highlighter:       spec.Highlighter,
		Prompt:            spec.Prompt,
		RPrompt:           spec.RPrompt,
		GlobalBindings:    spec.GlobalBindings,
		State:             spec.State,
	}
	if a.TTY == nil {
		a.TTY = NewTTY(os.Stdin, os.Stderr)
	}
	if a.MaxHeight == nil {
		a.MaxHeight = func() int { return -1 }
	}
	if a.RPromptPersistent == nil {
		a.RPromptPersistent = func() bool { return false }
	}
	if a.Highlighter == nil {
		a.Highlighter = dummyHighlighter{}
	}
	if a.Prompt == nil {
		a.Prompt = NewConstPrompt(nil)
	}
	if a.RPrompt == nil {
		a.RPrompt = NewConstPrompt(nil)
	}
	if a.GlobalBindings == nil {
		a.GlobalBindings = tk.DummyBindings{}
	}
	lp.HandleCb(a.handle)
	lp.RedrawCb(a.redraw)

	a.codeArea = tk.NewCodeArea(tk.CodeAreaSpec{
		Bindings:      spec.CodeAreaBindings,
		Highlighter:   a.Highlighter.Get,
		Prompt:        a.Prompt.Get,
		RPrompt:       a.RPrompt.Get,
		Abbreviations: spec.Abbreviations,
		QuotePaste:    spec.QuotePaste,
		OnSubmit:      a.CommitCode,
		State:         spec.CodeAreaState,

		SmallWordAbbreviations: spec.SmallWordAbbreviations,
	})

	return &a
}

func (a *app) MutateState(f func(*State)) {
	a.StateMutex.Lock()
	defer a.StateMutex.Unlock()
	f(&a.State)
}

func (a *app) CopyState() State {
	a.StateMutex.RLock()
	defer a.StateMutex.RUnlock()
	return a.State
}

func (a *app) CodeArea() tk.CodeArea {
	return a.codeArea
}

type closer interface {
	Close(bool)
}

func (a *app) SetAddon(w tk.Widget, accept bool) {
	a.StateMutex.Lock()
	defer a.StateMutex.Unlock()
	if c, ok := a.State.Addon.(closer); ok {
		c.Close(accept)
	}
	a.State.Addon = w
}

func (a *app) resetAllStates() {
	a.MutateState(func(s *State) { *s = State{} })
	a.codeArea.MutateState(
		func(s *tk.CodeAreaState) { *s = tk.CodeAreaState{} })
}

func (a *app) handle(e event) {
	switch e := e.(type) {
	case os.Signal:
		switch e {
		case syscall.SIGHUP:
			a.loop.Return("", io.EOF)
		case syscall.SIGINT:
			a.resetAllStates()
			a.triggerPrompts(true)
		case sys.SIGWINCH:
			a.RedrawFull()
		}
	case term.Event:
		var target tk.Widget
		if listing := a.CopyState().Addon; listing != nil {
			target = listing
		} else {
			target = a.codeArea
		}
		handled := target.Handle(e)
		if !handled {
			a.GlobalBindings.Handle(target, e)
		}
		if !a.loop.HasReturned() {
			a.triggerPrompts(false)
			a.reqRead <- struct{}{}
		}
	}
}

func (a *app) triggerPrompts(force bool) {
	a.Prompt.Trigger(force)
	a.RPrompt.Trigger(force)
}

func (a *app) redraw(flag redrawFlag) {
	// Get the dimensions available.
	height, width := a.TTY.Size()
	if maxHeight := a.MaxHeight(); maxHeight > 0 && maxHeight < height {
		height = maxHeight
	}

	var notes []string
	var addon tk.Renderer
	a.MutateState(func(s *State) {
		notes, addon = s.Notes, s.Addon
		s.Notes = nil
	})

	bufNotes := renderNotes(notes, width)
	isFinalRedraw := flag&finalRedraw != 0
	if isFinalRedraw {
		hideRPrompt := !a.RPromptPersistent()
		if hideRPrompt {
			a.codeArea.MutateState(func(s *tk.CodeAreaState) { s.HideRPrompt = true })
		}
		bufMain := renderApp(a.codeArea, nil /* addon */, width, height)
		if hideRPrompt {
			a.codeArea.MutateState(func(s *tk.CodeAreaState) { s.HideRPrompt = false })
		}
		// Insert a newline after the buffer and position the cursor there.
		bufMain.Extend(term.NewBuffer(width), true)

		a.TTY.UpdateBuffer(bufNotes, bufMain, flag&fullRedraw != 0)
		a.TTY.ResetBuffer()
	} else {
		bufMain := renderApp(a.codeArea, addon, width, height)
		a.TTY.UpdateBuffer(bufNotes, bufMain, flag&fullRedraw != 0)
	}
}

// Renders notes. This does not respect height so that overflow notes end up in
// the scrollback buffer.
func renderNotes(notes []string, width int) *term.Buffer {
	if len(notes) == 0 {
		return nil
	}
	bb := term.NewBufferBuilder(width)
	for i, note := range notes {
		if i > 0 {
			bb.Newline()
		}
		bb.Write(note)
	}
	return bb.Buffer()
}

type focuser interface {
	Focus() bool
}

// Renders the codearea, and uses the rest of the height for the listing.
func renderApp(codeArea, addon tk.Renderer, width, height int) *term.Buffer {
	buf := codeArea.Render(width, height)
	if addon != nil && len(buf.Lines) < height {
		bufListing := addon.Render(width, height-len(buf.Lines))
		focus := true
		if focuser, ok := addon.(focuser); ok {
			focus = focuser.Focus()
		}
		buf.Extend(bufListing, focus)
	}
	return buf
}

func (a *app) ReadCode() (string, error) {
	for _, f := range a.BeforeReadline {
		f()
	}
	defer func() {
		content := a.codeArea.CopyState().Buffer.Content
		for _, f := range a.AfterReadline {
			f(content)
		}
		a.resetAllStates()
	}()

	restore, err := a.TTY.Setup()
	if err != nil {
		return "", err
	}
	defer restore()

	var wg sync.WaitGroup
	defer wg.Wait()

	// Relay input events.
	a.reqRead = make(chan struct{}, 1)
	a.reqRead <- struct{}{}
	defer close(a.reqRead)
	defer a.TTY.StopInput()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range a.reqRead {
			event, err := a.TTY.ReadEvent()
			if err == nil {
				a.loop.Input(event)
			} else if err == term.ErrStopped {
				return
			} else if term.IsReadErrorRecoverable(err) {
				a.loop.Input(term.NonfatalErrorEvent{Err: err})
			} else {
				a.loop.Input(term.FatalErrorEvent{Err: err})
				return
			}
		}
	}()

	// Relay signals.
	sigCh := a.TTY.NotifySignals()
	defer a.TTY.StopSignals()
	wg.Add(1)
	go func() {
		for sig := range sigCh {
			a.loop.Input(sig)
		}
		wg.Done()
	}()

	// Relay late updates from prompt, rprompt and highlighter.
	stopRelayLateUpdates := make(chan struct{})
	defer close(stopRelayLateUpdates)
	relayLateUpdates := func(ch <-chan struct{}) {
		if ch == nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ch:
					a.Redraw()
				case <-stopRelayLateUpdates:
					return
				}
			}
		}()
	}

	relayLateUpdates(a.Prompt.LateUpdates())
	relayLateUpdates(a.RPrompt.LateUpdates())
	relayLateUpdates(a.Highlighter.LateUpdates())

	// Trigger an initial prompt update.
	a.triggerPrompts(true)

	return a.loop.Run()
}

func (a *app) Redraw() {
	a.loop.Redraw(false)
}

func (a *app) RedrawFull() {
	a.loop.Redraw(true)
}

func (a *app) CommitEOF() {
	a.loop.Return("", io.EOF)
}

func (a *app) CommitCode() {
	code := a.codeArea.CopyState().Buffer.Content
	a.loop.Return(code, nil)
}

func (a *app) Notify(note string) {
	a.MutateState(func(s *State) { s.Notes = append(s.Notes, note) })
	a.Redraw()
}
