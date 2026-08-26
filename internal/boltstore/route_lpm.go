package boltstore

import (
	"encoding/binary"
	"errors"
	"math/bits"
	"net/netip"
)

const (
	routeLPMHeaderSize = 16
	routeLPMNodeSize   = 33
)

var (
	routeLPMMagic   = [4]byte{'M', 'L', 'P', 'M'}
	routeLPMIPv4Key = []byte("ipv4")
	routeLPMIPv6Key = []byte("ipv6")
)

type RouteLPMStats struct {
	Nodes uint64
	IDs   uint64
	Bytes uint64
}

type routeLPMNode struct {
	prefix       Address
	prefixLength uint8
	children     [2]uint32 // one-based node indexes; zero means absent
	ids          []uint64
}

// routeLPMBuilder builds a Patricia trie from sorted or unsorted route-prefix
// groups. It keeps branch nodes only where prefixes diverge, rather than one
// node per address bit.
type routeLPMBuilder struct {
	version uint8
	nodes   []routeLPMNode
}

func newRouteLPMBuilder(version uint8) *routeLPMBuilder {
	return &routeLPMBuilder{
		version: version,
		nodes:   []routeLPMNode{{prefix: Address{}, prefixLength: 0}},
	}
}

func (builder *routeLPMBuilder) Insert(prefix netip.Prefix, ids []uint64) error {
	prefix = prefix.Masked()
	if builder.version == 4 && !prefix.Addr().Is4() {
		return errors.New("IPv6 prefix added to IPv4 route LPM")
	}
	if builder.version == 6 && (!prefix.Addr().Is6() || prefix.Addr().Zone() != "") {
		return errors.New("IPv4 or scoped prefix added to IPv6 route LPM")
	}
	if len(ids) == 0 {
		return errors.New("route LPM prefix has no route IDs")
	}
	address := AddressFromAddr(prefix.Addr())
	root, err := builder.insert(1, address, uint8(prefix.Bits()), ids)
	if err != nil {
		return err
	}
	if root != 1 {
		return errors.New("route LPM root was replaced")
	}
	return nil
}

func (builder *routeLPMBuilder) insert(reference uint32, prefix Address, prefixLength uint8, ids []uint64) (uint32, error) {
	if reference == 0 {
		return builder.appendNode(routeLPMNode{prefix: maskedRouteLPMAddress(prefix, builder.version, int(prefixLength)), prefixLength: prefixLength, ids: ids})
	}
	if int(reference) > len(builder.nodes) {
		return 0, errors.New("route LPM references a missing node")
	}
	index := reference - 1
	node := builder.nodes[index]
	common := routeLPMCommonPrefix(node.prefix, node.prefixLength, prefix, prefixLength, builder.version)
	if common == node.prefixLength {
		if common == prefixLength {
			if node.ids != nil {
				return 0, errors.New("duplicate route LPM prefix")
			}
			builder.nodes[index].ids = ids
			return reference, nil
		}
		branch := routeLPMBit(prefix, builder.version, int(common))
		child, err := builder.insert(node.children[branch], prefix, prefixLength, ids)
		if err != nil {
			return 0, err
		}
		builder.nodes[index].children[branch] = child
		return reference, nil
	}

	parent := routeLPMNode{
		prefix:       maskedRouteLPMAddress(prefix, builder.version, int(common)),
		prefixLength: common,
	}
	parentReference, err := builder.appendNode(parent)
	if err != nil {
		return 0, err
	}
	existingBranch := routeLPMBit(node.prefix, builder.version, int(common))
	builder.nodes[parentReference-1].children[existingBranch] = reference
	if common == prefixLength {
		builder.nodes[parentReference-1].ids = ids
		return parentReference, nil
	}
	newBranch := routeLPMBit(prefix, builder.version, int(common))
	child, err := builder.insert(0, prefix, prefixLength, ids)
	if err != nil {
		return 0, err
	}
	builder.nodes[parentReference-1].children[newBranch] = child
	return parentReference, nil
}

func (builder *routeLPMBuilder) appendNode(node routeLPMNode) (uint32, error) {
	if uint64(len(builder.nodes)) >= uint64(^uint32(0)) {
		return 0, errors.New("route LPM has too many nodes")
	}
	builder.nodes = append(builder.nodes, node)
	return uint32(len(builder.nodes)), nil
}

func (builder *routeLPMBuilder) Encode() ([]byte, RouteLPMStats, error) {
	var totalIDs uint64
	for _, node := range builder.nodes {
		totalIDs += uint64(len(node.ids))
	}
	if totalIDs > uint64(^uint32(0)) {
		return nil, RouteLPMStats{}, errors.New("route LPM has too many route IDs")
	}
	totalBytes := uint64(routeLPMHeaderSize) + uint64(len(builder.nodes))*routeLPMNodeSize + totalIDs*8
	if totalBytes > uint64(int(^uint(0)>>1)) {
		return nil, RouteLPMStats{}, errors.New("route LPM exceeds addressable memory")
	}
	encoded := make([]byte, int(totalBytes))
	copy(encoded[:4], routeLPMMagic[:])
	encoded[4] = builder.version
	binary.BigEndian.PutUint32(encoded[8:12], uint32(len(builder.nodes)))
	binary.BigEndian.PutUint32(encoded[12:16], uint32(totalIDs))

	nodeOffset := routeLPMHeaderSize
	idOffset := uint32(0)
	idsOffset := routeLPMHeaderSize + len(builder.nodes)*routeLPMNodeSize
	for _, node := range builder.nodes {
		copy(encoded[nodeOffset:nodeOffset+16], node.prefix[:])
		encoded[nodeOffset+16] = node.prefixLength
		binary.BigEndian.PutUint32(encoded[nodeOffset+17:nodeOffset+21], node.children[0])
		binary.BigEndian.PutUint32(encoded[nodeOffset+21:nodeOffset+25], node.children[1])
		binary.BigEndian.PutUint32(encoded[nodeOffset+25:nodeOffset+29], idOffset)
		binary.BigEndian.PutUint32(encoded[nodeOffset+29:nodeOffset+33], uint32(len(node.ids)))
		for _, id := range node.ids {
			binary.BigEndian.PutUint64(encoded[idsOffset:idsOffset+8], id)
			idsOffset += 8
		}
		idOffset += uint32(len(node.ids))
		nodeOffset += routeLPMNodeSize
	}
	return encoded, RouteLPMStats{Nodes: uint64(len(builder.nodes)), IDs: totalIDs, Bytes: totalBytes}, nil
}

func RouteLPMKey(version uint8) []byte {
	if version == 4 {
		return routeLPMIPv4Key
	}
	return routeLPMIPv6Key
}

// LookupRouteLPM resolves the most-specific matching route prefix from the
// serialized immutable trie. It returns every route object registered at that
// prefix, preserving multi-origin route responses.
func LookupRouteLPM(value []byte, address netip.Addr) ([]uint64, error) {
	version := uint8(6)
	if address.Is4() {
		version = 4
	}
	if len(value) < routeLPMHeaderSize || value[0] != routeLPMMagic[0] || value[1] != routeLPMMagic[1] || value[2] != routeLPMMagic[2] || value[3] != routeLPMMagic[3] || value[4] != version {
		return nil, errors.New("invalid route LPM header")
	}
	nodeCount := binary.BigEndian.Uint32(value[8:12])
	idCount := binary.BigEndian.Uint32(value[12:16])
	if nodeCount == 0 {
		return nil, errors.New("route LPM has no root node")
	}
	expected := uint64(routeLPMHeaderSize) + uint64(nodeCount)*routeLPMNodeSize + uint64(idCount)*8
	if expected != uint64(len(value)) {
		return nil, errors.New("invalid route LPM length")
	}
	query := AddressFromAddr(address)
	bits := 128
	if version == 4 {
		bits = 32
	}
	current := uint32(0)
	var bestOffset, bestCount uint32
	for {
		node, err := decodeRouteLPMNode(value, current, nodeCount)
		if err != nil {
			return nil, err
		}
		if !routeLPMAddressMatches(query, node.prefix, version, int(node.prefixLength)) {
			break
		}
		if node.idCount > 0 {
			bestOffset, bestCount = node.idOffset, node.idCount
		}
		if int(node.prefixLength) >= bits {
			break
		}
		branch := routeLPMBit(query, version, int(node.prefixLength))
		next := node.children[branch]
		if next == 0 {
			break
		}
		current = next - 1
	}
	if bestCount == 0 {
		return nil, nil
	}
	if uint64(bestOffset)+uint64(bestCount) > uint64(idCount) {
		return nil, errors.New("route LPM route ID range is invalid")
	}
	idsOffset := routeLPMHeaderSize + int(nodeCount)*routeLPMNodeSize + int(bestOffset)*8
	result := make([]uint64, bestCount)
	for index := range result {
		result[index] = binary.BigEndian.Uint64(value[idsOffset+index*8 : idsOffset+index*8+8])
	}
	return result, nil
}

type decodedRouteLPMNode struct {
	prefix       Address
	prefixLength uint8
	children     [2]uint32
	idOffset     uint32
	idCount      uint32
}

func decodeRouteLPMNode(value []byte, index, count uint32) (decodedRouteLPMNode, error) {
	if index >= count {
		return decodedRouteLPMNode{}, errors.New("route LPM child references a missing node")
	}
	offset := routeLPMHeaderSize + int(index)*routeLPMNodeSize
	node := decodedRouteLPMNode{prefixLength: value[offset+16]}
	copy(node.prefix[:], value[offset:offset+16])
	node.children[0] = binary.BigEndian.Uint32(value[offset+17 : offset+21])
	node.children[1] = binary.BigEndian.Uint32(value[offset+21 : offset+25])
	node.idOffset = binary.BigEndian.Uint32(value[offset+25 : offset+29])
	node.idCount = binary.BigEndian.Uint32(value[offset+29 : offset+33])
	return node, nil
}

func routeLPMCommonPrefix(left Address, leftLength uint8, right Address, rightLength uint8, version uint8) uint8 {
	limit := int(leftLength)
	if int(rightLength) < limit {
		limit = int(rightLength)
	}
	offset := 0
	if version == 4 {
		offset = 12
	}
	fullBytes := limit / 8
	for index := 0; index < fullBytes; index++ {
		difference := left[offset+index] ^ right[offset+index]
		if difference != 0 {
			return uint8(index*8 + bits.LeadingZeros8(uint8(difference)))
		}
	}
	for bit := fullBytes * 8; bit < limit; bit++ {
		if routeLPMBit(left, version, bit) != routeLPMBit(right, version, bit) {
			return uint8(bit)
		}
	}
	return uint8(limit)
}

func routeLPMBit(value Address, version uint8, bit int) uint32 {
	offset := 0
	if version == 4 {
		offset = 12
	}
	return uint32((value[offset+bit/8] >> uint(7-bit%8)) & 1)
}

func maskedRouteLPMAddress(value Address, version uint8, length int) Address {
	bits := 128
	offset := 0
	if version == 4 {
		bits, offset = 32, 12
	}
	if length >= bits {
		return value
	}
	fullBytes, remainder := length/8, length%8
	if remainder != 0 {
		value[offset+fullBytes] &= 0xff << uint(8-remainder)
		fullBytes++
	}
	for index := offset + fullBytes; index < offset+bits/8; index++ {
		value[index] = 0
	}
	return value
}

func routeLPMAddressMatches(address, prefix Address, version uint8, length int) bool {
	return routeLPMCommonPrefix(address, uint8(length), prefix, uint8(length), version) == uint8(length)
}
