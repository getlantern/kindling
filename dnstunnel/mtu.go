package dnstunnel

// frameHeaderMax bounds the inner header (version+kind+flags+stream_id+seq+frag+comp), so a sealed
// data frame always fits its DNS carrier.
const frameHeaderMax = 16

// wireOverhead is the per-packet cost of a data frame: form + ConnectionID + nonce + AEAD tag.
const wireOverhead = 1 + connIDLen + nonceLen + tagLen

// maxBase32Chars is how many base32 chars fit in budget QNAME bytes, paying one length byte per
// ≤63-char label.
func maxBase32Chars(budget int) int {
	return budget/64*63 + max(budget%64-1, 0)
}

// uplinkSegment is the largest ARQ payload whose sealed frame base32-packs into a QNAME under zone.
func uplinkSegment(zone zoneName) int {
	wire := maxBase32Chars(maxNameLen-zone.wireLen()) * 5 / 8
	return max(wire-wireOverhead-frameHeaderMax, 1)
}
