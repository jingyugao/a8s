package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

type console struct {
	input    *bufio.Reader
	terminal *term.Terminal
}

type terminalIO struct{}

func (terminalIO) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (terminalIO) Write(p []byte) (int, error) { return os.Stdout.Write(p) }

func newConsole() *console {
	c := &console{input: bufio.NewReader(os.Stdin)}
	if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
		c.terminal = term.NewTerminal(terminalIO{}, "you > ")
		if width, height, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
			_ = c.terminal.SetSize(width, height)
		}
	}
	return c
}

func (c *console) Write(p []byte) (int, error) { return os.Stdout.Write(p) }

func (c *console) read(prompt string) (string, error) {
	if c.terminal == nil {
		fmt.Fprint(c, prompt)
		line, err := c.input.ReadString('\n')
		if err == io.EOF && line != "" {
			err = nil
		}
		return strings.TrimRight(line, "\r\n"), err
	}
	previous, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return "", err
	}
	defer term.Restore(int(os.Stdin.Fd()), previous)
	c.terminal.SetPrompt(prompt)
	c.terminal.SetBracketedPasteMode(true)
	defer c.terminal.SetBracketedPasteMode(false)
	line, err := c.terminal.ReadLine()
	if err == term.ErrPasteIndicator {
		err = nil
	}
	return line, err
}

// Model output and stored messages must not control the operator's terminal.
func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		if (r < 32 && r != '\n' && r != '\t') || (r >= 127 && r <= 159) {
			return -1
		}
		return r
	}, s)
}
