package terminal

import (
	"bytes"
	"testing"
)

func assertFeed(t *testing.T, detach bool, fwd []byte, wantDetach bool, wantFwd []byte) {
	t.Helper()
	if detach != wantDetach {
		t.Errorf("detach: got %v, want %v", detach, wantDetach)
	}
	if !bytes.Equal(fwd, wantFwd) {
		t.Errorf("fwd: got %v, want %v", fwd, wantFwd)
	}
}

func assertBytes(t *testing.T, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Errorf("bytes: got %v, want %v", got, want)
	}
}

func TestDetachSequence(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.Feed(0x02)
	assertFeed(t, detach, fwd, false, nil)
	detach, fwd = d.Feed('d')
	assertFeed(t, detach, fwd, true, nil)
}

func TestNotDetachForwardsPrefix(t *testing.T) {
	d := NewDetachDetector()
	d.Feed(0x02)
	detach, fwd := d.Feed('x')
	assertFeed(t, detach, fwd, false, []byte{0x02, 'x'})
}

func TestRegularBytesPassThrough(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.Feed('a')
	assertFeed(t, detach, fwd, false, []byte{'a'})
	detach, fwd = d.Feed('b')
	assertFeed(t, detach, fwd, false, []byte{'b'})
}

func TestFeedBufDetach(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("hello\x02d"))
	if !detach {
		t.Fatal("expected detach")
	}
	assertBytes(t, fwd, []byte("hello"))
}

func TestDetachWithInterleavedCursorReport(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("\x02\x1b[24;80Rd"))
	if !detach {
		t.Fatal("expected detach")
	}
	assertBytes(t, fwd, []byte("\x1b[24;80R"))
}

func TestDetachWithFocusEvent(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("\x02\x1b[Id"))
	if !detach {
		t.Fatal("expected detach")
	}
	assertBytes(t, fwd, []byte("\x1b[I"))
}

func TestDetachWithFocusOutEvent(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("\x02\x1b[Od"))
	if !detach {
		t.Fatal("expected detach")
	}
	assertBytes(t, fwd, []byte("\x1b[O"))
}

func TestDetachWithMultipleEscapeSequences(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("\x02\x1b[I\x1b[24;80Rd"))
	if !detach {
		t.Fatal("expected detach")
	}
	assertBytes(t, fwd, []byte("\x1b[I\x1b[24;80R"))
}

func TestDetachWithMouseReport(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("\x02\x1b[<0;10;20Md"))
	if !detach {
		t.Fatal("expected detach")
	}
	assertBytes(t, fwd, []byte("\x1b[<0;10;20M"))
}

func TestDetachKittyCtrlBRawD(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("\x1b[98;5ud"))
	if !detach {
		t.Fatal("expected detach")
	}
	assertBytes(t, fwd, nil)
}

func TestDetachKittyCtrlBKittyD(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("\x1b[98;5u\x1b[100;1u"))
	if !detach {
		t.Fatal("expected detach")
	}
	assertBytes(t, fwd, nil)
}

func TestDetachKittyCtrlBKittyDNoModifier(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("\x1b[98;5u\x1b[100u"))
	if !detach {
		t.Fatal("expected detach")
	}
	assertBytes(t, fwd, nil)
}

func TestDetachLegacyCtrlBKittyD(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("\x02\x1b[100;1u"))
	if !detach {
		t.Fatal("expected detach")
	}
	assertBytes(t, fwd, nil)
}

func TestKittyCtrlBWithInterleavedEscape(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("\x1b[98;5u\x1b[Id"))
	if !detach {
		t.Fatal("expected detach")
	}
	assertBytes(t, fwd, []byte("\x1b[I"))
}

func TestKittyCtrlBFocusThenKittyD(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("\x1b[98;5u\x1b[I\x1b[100;1u"))
	if !detach {
		t.Fatal("expected detach")
	}
	assertBytes(t, fwd, []byte("\x1b[I"))
}

func TestKittyNonCtrlBPassesThrough(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("\x1b[97;1u"))
	if detach {
		t.Fatal("should not detach")
	}
	assertBytes(t, fwd, []byte("\x1b[97;1u"))
}

func TestKittyCtrlBThenKittyNonDCancels(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("\x1b[98;5u\x1b[120;1u"))
	if detach {
		t.Fatal("should not detach")
	}
	assertBytes(t, fwd, []byte{0x02, 0x1b, '[', '1', '2', '0', ';', '1', 'u'})
}

func TestKittyCtrlDNotDetach(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("\x1b[98;5u\x1b[100;5u"))
	if detach {
		t.Fatal("should not detach")
	}
	assertBytes(t, fwd, []byte{0x02, 0x1b, '[', '1', '0', '0', ';', '5', 'u'})
}

func TestEscapeInNormalStatePassesThrough(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("\x1b[6n"))
	if detach {
		t.Fatal("should not detach")
	}
	assertBytes(t, fwd, []byte("\x1b[6n"))
}

func TestTwoCharEscapeInSawPrefix(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("\x02\x1bNd"))
	if !detach {
		t.Fatal("expected detach")
	}
	assertBytes(t, fwd, []byte("\x1bN"))
}

func TestNormalTextBetweenDetachAttempts(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("abc\x02d"))
	if !detach {
		t.Fatal("expected detach")
	}
	assertBytes(t, fwd, []byte("abc"))
}

func TestKittyCtrlBCancelRaw(t *testing.T) {
	d := NewDetachDetector()
	detach, fwd := d.FeedBuf([]byte("\x1b[98;5ux"))
	if detach {
		t.Fatal("should not detach")
	}
	assertBytes(t, fwd, []byte{0x02, 'x'})
}
