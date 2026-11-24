package reader

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
)

// TranscriptOffset identifies the file and EOF observed before sending input.
// A nil offset means that no send-time coverage was captured (legacy records
// or an unavailable transcript). Offset zero is a valid snapshot of an empty file.
type TranscriptOffset struct {
	Path        string `json:"path"`
	Offset      int64  `json:"offset"`
	Device      uint64 `json:"device,omitempty"`
	Inode       uint64 `json:"inode,omitempty"`
	PrefixBytes int64  `json:"prefix_bytes,omitempty"`
	PrefixHash  string `json:"prefix_hash,omitempty"`
	// AnchorBytes/AnchorHash hash the bytes just before Offset (at most
	// 4096). Device/inode and size alone accept a file truncated and regrown
	// past the offset, or a new file that reused the inode; the bytes before
	// the offset of an append-only transcript never change.
	AnchorBytes int64  `json:"anchor_bytes,omitempty"`
	AnchorHash  string `json:"anchor_hash,omitempty"`
}

const transcriptIdentityBytes int64 = 4096

// CaptureTranscriptOffset takes size and identity from the same open file.
// Failure is conservative: delivery can proceed, but gets no offset coverage.
func CaptureTranscriptOffset(path string) *TranscriptOffset {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	dev, ino := FileIdentity(info)
	snapshot := &TranscriptOffset{Path: path, Offset: info.Size(), Device: dev, Inode: ino}
	if snapshot.Offset > 0 {
		snapshot.AnchorBytes = min(snapshot.Offset, transcriptIdentityBytes)
		snapshot.AnchorHash = transcriptHash(f, snapshot.Offset-snapshot.AnchorBytes, snapshot.AnchorBytes)
		if snapshot.AnchorHash == "" {
			return nil
		}
	}
	if ino == 0 {
		snapshot.PrefixBytes = min(info.Size(), transcriptIdentityBytes)
		// An empty prefix cannot establish any file identity.
		if snapshot.PrefixBytes == 0 {
			return nil
		}
		snapshot.PrefixHash = transcriptPrefixHash(f, snapshot.PrefixBytes)
		if snapshot.PrefixHash == "" {
			return nil
		}
	}
	if current, err := os.Stat(path); err != nil || !os.SameFile(info, current) || current.Size() < snapshot.Offset {
		return nil
	}
	return snapshot
}

// Matches validates the opened scan source against the send-time snapshot.
// Prefix hashes are used only on platforms/filesystems without an inode.
func (s *TranscriptOffset) Matches(f *os.File, path string, info os.FileInfo) bool {
	if s == nil || s.Path == "" || s.Path != path || s.Offset < 0 || !info.Mode().IsRegular() || info.Size() < s.Offset {
		return false
	}
	// Offset zero puts the whole file in the window; anything else must
	// still have the same bytes just before the offset.
	if s.Offset > 0 && (s.AnchorBytes != min(s.Offset, transcriptIdentityBytes) || s.AnchorHash == "" ||
		s.AnchorHash != transcriptHash(f, s.Offset-s.AnchorBytes, s.AnchorBytes)) {
		return false
	}
	if s.Inode != 0 {
		dev, ino := FileIdentity(info)
		return s.Device == dev && s.Inode == ino
	}
	return s.PrefixBytes > 0 && s.PrefixBytes <= transcriptIdentityBytes && s.PrefixBytes <= s.Offset &&
		s.PrefixHash != "" && s.PrefixHash == transcriptPrefixHash(f, s.PrefixBytes)
}

func transcriptPrefixHash(f *os.File, size int64) string {
	return transcriptHash(f, 0, size)
}

func transcriptHash(f *os.File, from, size int64) string {
	b := make([]byte, size)
	if _, err := f.ReadAt(b, from); err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
