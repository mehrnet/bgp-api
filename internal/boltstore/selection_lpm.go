package boltstore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net/netip"
)

const selectionLPMHeaderSize = 16

var selectionLPMMagic = [4]byte{'S', 'L', 'P', '2'}

// SelectionLPMStats describes one immutable allocation or geofeed selector.
type SelectionLPMStats struct {
	Nodes uint64
	Bytes uint64
}

type selectionCandidate struct {
	id    uint32
	width Address
}

type selectionLPMNode struct {
	prefix       Address
	prefixLength uint8
	children     [2]uint32 // one-based node indexes; zero means absent
	candidate    selectionCandidate
}

// selectionLPMBuilder creates a compact Patricia trie whose lookup returns
// precisely the record selected by the compact IP response contract.
type selectionLPMBuilder struct {
	version uint8
	nodes   []selectionLPMNode
}

func SelectionLPMKey(version uint8) []byte { return RouteLPMKey(version) }

func newSelectionLPMBuilder(version uint8) *selectionLPMBuilder {
	return &selectionLPMBuilder{
		version: version,
		nodes:   []selectionLPMNode{{prefix: Address{}, prefixLength: 0}},
	}
}

func (builder *selectionLPMBuilder) Insert(prefix netip.Prefix, id uint64, start, end Address) error {
	prefix = prefix.Masked()
	if builder.version == 4 && !prefix.Addr().Is4() {
		return errors.New("IPv6 prefix added to IPv4 selection LPM")
	}
	if builder.version == 6 && (!prefix.Addr().Is6() || prefix.Addr().Zone() != "") {
		return errors.New("IPv4 or scoped prefix added to IPv6 selection LPM")
	}
	if id == 0 {
		return errors.New("selection LPM record has no ID")
	}
	if id > uint64(^uint32(0)) {
		return errors.New("selection LPM record ID exceeds uint32")
	}
	candidate := selectionCandidate{id: uint32(id), width: selectionRangeWidth(start, end)}
	address := AddressFromAddr(prefix.Addr())
	root, err := builder.insert(1, address, uint8(prefix.Bits()), candidate)
	if err != nil {
		return err
	}
	if root != 1 {
		return errors.New("selection LPM root was replaced")
	}
	return nil
}

func (builder *selectionLPMBuilder) insert(reference uint32, prefix Address, prefixLength uint8, candidate selectionCandidate) (uint32, error) {
	if reference == 0 {
		return builder.appendNode(selectionLPMNode{prefix: maskedRouteLPMAddress(prefix, builder.version, int(prefixLength)), prefixLength: prefixLength, candidate: candidate})
	}
	if int(reference) > len(builder.nodes) {
		return 0, errors.New("selection LPM references a missing node")
	}
	index := reference - 1
	node := builder.nodes[index]
	common := routeLPMCommonPrefix(node.prefix, node.prefixLength, prefix, prefixLength, builder.version)
	if common == node.prefixLength {
		if common == prefixLength {
			if node.candidate.id == 0 || selectionCandidateLess(candidate, node.candidate) {
				builder.nodes[index].candidate = candidate
			}
			return reference, nil
		}
		branch := routeLPMBit(prefix, builder.version, int(common))
		child, err := builder.insert(node.children[branch], prefix, prefixLength, candidate)
		if err != nil {
			return 0, err
		}
		builder.nodes[index].children[branch] = child
		return reference, nil
	}

	parent := selectionLPMNode{
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
		builder.nodes[parentReference-1].candidate = candidate
		return parentReference, nil
	}
	newBranch := routeLPMBit(prefix, builder.version, int(common))
	child, err := builder.insert(0, prefix, prefixLength, candidate)
	if err != nil {
		return 0, err
	}
	builder.nodes[parentReference-1].children[newBranch] = child
	return parentReference, nil
}

func (builder *selectionLPMBuilder) appendNode(node selectionLPMNode) (uint32, error) {
	if uint64(len(builder.nodes)) >= uint64(^uint32(0)) {
		return 0, errors.New("selection LPM has too many nodes")
	}
	builder.nodes = append(builder.nodes, node)
	return uint32(len(builder.nodes)), nil
}

func (builder *selectionLPMBuilder) Encode() ([]byte, SelectionLPMStats, error) {
	nodeSize, addressBytes, err := selectionLPMNodeLayout(builder.version)
	if err != nil {
		return nil, SelectionLPMStats{}, err
	}
	totalBytes := uint64(selectionLPMHeaderSize) + uint64(len(builder.nodes))*uint64(nodeSize)
	if totalBytes > uint64(int(^uint(0)>>1)) {
		return nil, SelectionLPMStats{}, errors.New("selection LPM exceeds addressable memory")
	}
	encoded := make([]byte, int(totalBytes))
	copy(encoded[:4], selectionLPMMagic[:])
	encoded[4] = builder.version
	binary.BigEndian.PutUint32(encoded[8:12], uint32(len(builder.nodes)))

	for index, node := range builder.nodes {
		offset := selectionLPMHeaderSize + index*nodeSize
		copy(encoded[offset:offset+addressBytes], selectionAddressBytes(node.prefix, builder.version))
		offset += addressBytes
		encoded[offset] = node.prefixLength
		offset++
		binary.BigEndian.PutUint32(encoded[offset:offset+4], node.children[0])
		binary.BigEndian.PutUint32(encoded[offset+4:offset+8], node.children[1])
		offset += 8
		binary.BigEndian.PutUint32(encoded[offset:offset+4], node.candidate.id)
		offset += 4
		copy(encoded[offset:offset+addressBytes], selectionAddressBytes(node.candidate.width, builder.version))
	}
	return encoded, SelectionLPMStats{Nodes: uint64(len(builder.nodes)), Bytes: totalBytes}, nil
}

// LookupSelectionLPM returns the same allocation or geofeed record that the
// compact API would select after examining every matching source range.
func LookupSelectionLPM(value []byte, address netip.Addr) (uint64, error) {
	version := uint8(6)
	if address.Is4() {
		version = 4
	}
	nodeSize, addressBytes, err := selectionLPMNodeLayout(version)
	if err != nil {
		return 0, err
	}
	if len(value) < selectionLPMHeaderSize || value[0] != selectionLPMMagic[0] || value[1] != selectionLPMMagic[1] || value[2] != selectionLPMMagic[2] || value[3] != selectionLPMMagic[3] || value[4] != version {
		return 0, errors.New("invalid selection LPM header")
	}
	nodeCount := binary.BigEndian.Uint32(value[8:12])
	if nodeCount == 0 {
		return 0, errors.New("selection LPM has no root node")
	}
	expected := uint64(selectionLPMHeaderSize) + uint64(nodeCount)*uint64(nodeSize)
	if expected != uint64(len(value)) {
		return 0, errors.New("invalid selection LPM length")
	}
	query := AddressFromAddr(address)
	bits := 128
	if version == 4 {
		bits = 32
	}
	current := uint32(0)
	var bestID uint32
	var bestWidth Address
	var bestPrefixLength uint8
	for {
		if current >= nodeCount {
			return 0, errors.New("selection LPM child references a missing node")
		}
		offset := selectionLPMHeaderSize + int(current)*nodeSize
		prefixLength := value[offset+addressBytes]
		if int(prefixLength) > bits || !selectionAddressMatches(query, value[offset:offset+addressBytes], version, int(prefixLength)) {
			break
		}
		candidateOffset := offset + addressBytes + 1 + 8
		candidateID := binary.BigEndian.Uint32(value[candidateOffset : candidateOffset+4])
		if candidateID != 0 {
			widthOffset := candidateOffset + 4
			if bestID == 0 || selectionEncodedCandidateLess(value[widthOffset:widthOffset+addressBytes], candidateID, prefixLength, bestWidth, bestID, bestPrefixLength, version) {
				bestID, bestPrefixLength = candidateID, prefixLength
				selectionCopyAddressBytes(bestWidth[:], value[widthOffset:widthOffset+addressBytes], version)
			}
		}
		if int(prefixLength) >= bits {
			break
		}
		branch := routeLPMBit(query, version, int(prefixLength))
		childrenOffset := offset + addressBytes + 1
		next := binary.BigEndian.Uint32(value[childrenOffset+int(branch)*4 : childrenOffset+int(branch)*4+4])
		if next == 0 {
			break
		}
		current = next - 1
	}
	return uint64(bestID), nil
}

func selectionLPMNodeLayout(version uint8) (nodeSize, addressBytes int, err error) {
	switch version {
	case 4:
		return 21, 4, nil
	case 6:
		return 45, 16, nil
	default:
		return 0, 0, errors.New("invalid selection LPM version")
	}
}

func selectionCandidateLess(left, right selectionCandidate) bool {
	if compared := bytes.Compare(left.width[:], right.width[:]); compared != 0 {
		return compared < 0
	}
	return left.id < right.id
}

func selectionEncodedCandidateLess(width []byte, id uint32, prefixLength uint8, bestWidth Address, bestID uint32, bestPrefixLength uint8, version uint8) bool {
	best := selectionAddressBytes(bestWidth, version)
	if compared := bytes.Compare(width, best); compared != 0 {
		return compared < 0
	}
	if prefixLength != bestPrefixLength {
		return prefixLength > bestPrefixLength
	}
	return id < bestID
}

func selectionRangeWidth(start, end Address) Address {
	var width Address
	borrow := 0
	for index := len(width) - 1; index >= 0; index-- {
		value := int(end[index]) - int(start[index]) - borrow
		if value < 0 {
			value += 256
			borrow = 1
		} else {
			borrow = 0
		}
		width[index] = byte(value)
	}
	return width
}

func selectionAddressBytes(value Address, version uint8) []byte {
	if version == 4 {
		return value[12:]
	}
	return value[:]
}

func selectionCopyAddressBytes(destination, source []byte, version uint8) {
	if version == 4 {
		copy(destination[12:], source)
		return
	}
	copy(destination, source)
}

func selectionAddressMatches(address Address, prefix []byte, version uint8, length int) bool {
	offset := 0
	if version == 4 {
		offset = 12
	}
	fullBytes, remainder := length/8, length%8
	if !bytes.Equal(address[offset:offset+fullBytes], prefix[:fullBytes]) {
		return false
	}
	if remainder == 0 {
		return true
	}
	mask := byte(0xff << uint(8-remainder))
	return address[offset+fullBytes]&mask == prefix[fullBytes]&mask
}
