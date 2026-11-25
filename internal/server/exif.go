package server

import (
	"encoding/binary"
	"time"
)

// exifTIFF returns the TIFF block of a JPEG's EXIF segment, or nil.
func exifTIFF(b []byte) []byte {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return nil
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return nil
		}
		marker := b[i+1]
		switch {
		case marker == 0xFF: // fill byte
			i++
			continue
		case marker == 0xDA || marker == 0xD9: // start of scan / end of image
			return nil
		case marker >= 0xD0 && marker <= 0xD7 || marker == 0x01: // standalone markers
			i += 2
			continue
		}
		segLen := int(b[i+2])<<8 | int(b[i+3])
		if segLen < 2 || i+2+segLen > len(b) {
			return nil
		}
		seg := b[i+4 : i+2+segLen]
		if marker == 0xE1 && len(seg) >= 6 && string(seg[:6]) == "Exif\x00\x00" {
			return seg[6:]
		}
		i += 2 + segLen
	}
	return nil
}

// exifOrientation returns the EXIF orientation (1-8) of a JPEG, or 1 if absent.
func exifOrientation(b []byte) int {
	t, ifd0, ok := parseTIFF(exifTIFF(b))
	if !ok {
		return 1
	}
	if typ, _, val, ok := t.find(ifd0, 0x0112); ok && typ == 3 { // SHORT
		if o := int(t.bo.Uint16(val)); o >= 1 && o <= 8 {
			return o
		}
	}
	return 1
}

// exifDateTaken returns when a JPEG was taken according to EXIF
// (DateTimeOriginal, then DateTimeDigitized, then DateTime), or the zero time.
// EXIF dates carry no time zone; the wall-clock value is returned as UTC.
func exifDateTaken(b []byte) time.Time {
	t, ifd0, ok := parseTIFF(exifTIFF(b))
	if !ok {
		return time.Time{}
	}
	if typ, _, val, ok := t.find(ifd0, 0x8769); ok && (typ == 4 || typ == 13) { // Exif IFD pointer
		sub := int(t.bo.Uint32(val))
		for _, tag := range []uint16{0x9003, 0x9004} {
			if d := t.date(sub, tag); !d.IsZero() {
				return d
			}
		}
	}
	return t.date(ifd0, 0x0132)
}

type tiffData struct {
	b  []byte
	bo binary.ByteOrder
}

// parseTIFF checks the TIFF header and returns the offset of IFD0.
func parseTIFF(b []byte) (tiffData, int, bool) {
	if len(b) < 8 {
		return tiffData{}, 0, false
	}
	t := tiffData{b: b}
	switch string(b[:2]) {
	case "II":
		t.bo = binary.LittleEndian
	case "MM":
		t.bo = binary.BigEndian
	default:
		return tiffData{}, 0, false
	}
	if t.bo.Uint16(b[2:]) != 42 {
		return tiffData{}, 0, false
	}
	return t, int(t.bo.Uint32(b[4:])), true
}

// find looks up tag in the IFD at off and returns its type, count and the
// 4-byte value/offset field.
func (t tiffData) find(off int, tag uint16) (typ uint16, count uint32, val []byte, ok bool) {
	if off < 8 || off+2 > len(t.b) {
		return 0, 0, nil, false
	}
	n := int(t.bo.Uint16(t.b[off:]))
	for k := range n {
		e := off + 2 + k*12
		if e+12 > len(t.b) {
			return 0, 0, nil, false
		}
		if t.bo.Uint16(t.b[e:]) == tag {
			return t.bo.Uint16(t.b[e+2:]), t.bo.Uint32(t.b[e+4:]), t.b[e+8 : e+12], true
		}
	}
	return 0, 0, nil, false
}

// date reads an EXIF "2006:01:02 15:04:05" ASCII tag.
func (t tiffData) date(ifd int, tag uint16) time.Time {
	typ, count, val, ok := t.find(ifd, tag)
	if !ok || typ != 2 || count < 19 {
		return time.Time{}
	}
	off := int(t.bo.Uint32(val))
	if off < 0 || off+19 > len(t.b) {
		return time.Time{}
	}
	d, err := time.Parse("2006:01:02 15:04:05", string(t.b[off:off+19]))
	if err != nil || d.Year() < 1900 || d.After(time.Now().AddDate(0, 0, 2)) {
		return time.Time{}
	}
	return d
}
