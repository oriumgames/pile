package format

// mortonKey interleaves the bits of two chunk coordinates into a Z-order
// (Morton) key. Coordinates are offset into unsigned space first so that the
// ordering is a total order over the full int32 range.
func mortonKey(x, z int32) uint64 {
	return spread(uint32(x)^0x80000000) | spread(uint32(z)^0x80000000)<<1
}

// spread distributes the 32 bits of v over the even bit positions of a uint64.
func spread(v uint32) uint64 {
	x := uint64(v)
	x = (x | x<<16) & 0x0000FFFF0000FFFF
	x = (x | x<<8) & 0x00FF00FF00FF00FF
	x = (x | x<<4) & 0x0F0F0F0F0F0F0F0F
	x = (x | x<<2) & 0x3333333333333333
	x = (x | x<<1) & 0x5555555555555555
	return x
}
