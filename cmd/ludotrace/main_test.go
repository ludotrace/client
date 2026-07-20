package main

import (
	"errors"
	"testing"
	"time"

	"github.com/ludotrace/client/internal/uploader"
)

func TestLimitReachedWait(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want time.Duration
	}{
		{
			name: "no hint falls back to default 60s",
			err:  &uploader.LimitReachedError{},
			want: limitReachedDefaultWait,
		},
		{
			name: "bare sentinel falls back to default 60s",
			err:  uploader.ErrLimitReached,
			want: limitReachedDefaultWait,
		},
		{
			name: "unrelated error falls back to default 60s",
			err:  errors.New("boom"),
			want: limitReachedDefaultWait,
		},
		{
			name: "valid hint is honored verbatim",
			err:  &uploader.LimitReachedError{RetryAfter: 12 * time.Hour},
			want: 12 * time.Hour,
		},
		{
			name: "hint below floor is clamped up to the minimum",
			err:  &uploader.LimitReachedError{RetryAfter: 1 * time.Second},
			want: limitReachedMinWait,
		},
		{
			name: "hint above cap is clamped down to the maximum",
			err:  &uploader.LimitReachedError{RetryAfter: 72 * time.Hour},
			want: limitReachedMaxWait,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := limitReachedWait(tt.err); got != tt.want {
				t.Fatalf("limitReachedWait = %v, want %v", got, tt.want)
			}
		})
	}
}
