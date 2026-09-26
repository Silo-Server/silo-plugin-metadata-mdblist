package provider

import "errors"

// Lookup failures. Silo has to tell each of these from "MDBList has nothing
// for this title": its bulk enrichment pass records an empty answer and does
// not ask about the title again for weeks, while every failure below means
// "ask again later". main.go maps each to a gRPC status. Errors carry detail
// by wrapping one of these, so match them with errors.Is.
var (
	// ErrNotConfigured means no API key is set.
	ErrNotConfigured = errors.New("mdblist: no API key configured")
	// ErrKeyRejected means MDBList, or Cloudflare in front of it, refused the
	// key.
	ErrKeyRejected = errors.New("mdblist: API key rejected")
	// ErrQuotaExhausted means the daily quota or the five-minute burst limit
	// is spent, and the client is paused until it resets.
	ErrQuotaExhausted = errors.New("mdblist: request quota spent")
	// ErrUnavailable means MDBList could not be reached or failed to answer.
	ErrUnavailable = errors.New("mdblist: service unavailable")
	// ErrUnusableAnswer means MDBList refused or garbled the answer for this
	// one title. Unlike the others, it says nothing about other titles.
	ErrUnusableAnswer = errors.New("mdblist: unusable answer for this title")
)
