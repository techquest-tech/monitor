package db

import (
	"errors"
	"testing"
	"time"

	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/stretchr/testify/assert"
)

func TestBuildErrorModel(t *testing.T) {
	svc := &TracingRequestServiceDBImpl{}

	t.Run("normal report", func(t *testing.T) {
		when := time.Date(2026, 8, 21, 10, 0, 0, 0, time.Local)
		m, keep := svc.buildErrorModel(core.ErrorReport{
			Uri:       "/v1/descente/decode?epc=30340BFB40331082CB417C9E",
			FullStack: []byte("goroutine 1 [running]: ..."),
			Error:     errors.New("runtime error: invalid memory address or nil pointer dereference"),
			HappendAT: when,
		})
		assert.True(t, keep)
		if assert.NotNil(t, m) {
			assert.Equal(t, "/v1/descente/decode?epc=30340BFB40331082CB417C9E", m.Uri)
			assert.Equal(t, "runtime error: invalid memory address or nil pointer dereference", m.ErrorText)
			assert.Equal(t, when, m.HappendAT)
			assert.Equal(t, "goroutine 1 [running]: ...", string(m.FullStack))
		}
	})

	t.Run("zero happend_at fallback to now", func(t *testing.T) {
		m, keep := svc.buildErrorModel(core.ErrorReport{
			Error: errors.New("boom"),
		})
		assert.True(t, keep)
		if assert.NotNil(t, m) {
			assert.False(t, m.HappendAT.IsZero())
		}
	})

	t.Run("empty report skipped", func(t *testing.T) {
		m, keep := svc.buildErrorModel(core.ErrorReport{})
		assert.False(t, keep)
		assert.Nil(t, m)
	})

	t.Run("truncation", func(t *testing.T) {
		long := string(make([]byte, 0)) + repeat('x', 3000)
		m, keep := svc.buildErrorModel(core.ErrorReport{
			Uri:       repeat('u', 300),
			Error:     errors.New(long),
		})
		assert.True(t, keep)
		if assert.NotNil(t, m) {
			assert.Equal(t, 256, len([]rune(m.Uri)))
			assert.Equal(t, 2048, len([]rune(m.ErrorText)))
		}
	})
}

func repeat(r rune, n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = r
	}
	return string(b)
}
