package profile

// palette is a fixed set of eight visually distinct dark background colors
// expressed as six-character lowercase hex strings. The colors are chosen to
// be sufficiently separated in hue and brightness to remain distinguishable in
// terminal emulators that render 24-bit color.
var palette = []string{
	"0a1628", // dark navy
	"051a05", // dark green
	"1a0a0a", // dark burgundy
	"0a1a1a", // dark teal
	"1a0a1a", // dark purple
	"1a1a05", // dark olive
	"0a0a1a", // dark indigo
	"1a0f05", // dark brown
}

// AutoColor returns a deterministic palette color for a given profile index.
// The index wraps around so that any non-negative integer maps to a valid entry.
func AutoColor(index int) string {
	return palette[index%len(palette)]
}
