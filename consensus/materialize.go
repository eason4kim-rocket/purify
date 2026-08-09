package consensus

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

const (
	// MaxMaterializedDataBytes bounds the merged JSON document independently
	// from the richer field-consensus response.
	MaxMaterializedDataBytes = 4 << 20
)

// MaterializationStatus describes whether a single merged JSON document could
// be selected without breaking a consensus tie.
type MaterializationStatus string

const (
	MaterializationStatusComplete  MaterializationStatus = "complete"
	MaterializationStatusAmbiguous MaterializationStatus = "ambiguous"
)

// Materialization is a typed merged document. Data is present only when Status
// is complete; ambiguity is not an error.
type Materialization struct {
	Status MaterializationStatus `json:"status"`
	Data   json.RawMessage       `json:"data,omitempty"`
}

type materialNodeKind uint8

const (
	materialNodeObject materialNodeKind = iota
	materialNodeArray
	materialNodeScalar
)

type materialNode struct {
	sourceID int
	value    any
}

type materialBuilder struct {
	data  []byte
	probe bool
}

func (builder *materialBuilder) appendBytes(value []byte) error {
	if builder == nil {
		return fmt.Errorf("%w: materialization builder is nil", ErrInvalidInput)
	}
	if builder.probe {
		return nil
	}
	if len(builder.data) > MaxMaterializedDataBytes || len(value) > MaxMaterializedDataBytes-len(builder.data) {
		return fmt.Errorf("%w: materialized document exceeds %d bytes", ErrResourceLimit, MaxMaterializedDataBytes)
	}
	builder.data = append(builder.data, value...)
	return nil
}

func (builder *materialBuilder) appendByte(value byte) error {
	if builder != nil && builder.probe {
		return nil
	}
	return builder.appendBytes([]byte{value})
}

func (builder *materialBuilder) appendJSONString(value string) error {
	if builder == nil {
		return fmt.Errorf("%w: materialization builder is nil", ErrInvalidInput)
	}
	if builder.probe {
		return nil
	}
	return builder.appendBytes(canonicalJSONString(value))
}

func (builder *materialBuilder) appendScalar(value any) error {
	if builder == nil {
		return fmt.Errorf("%w: materialization builder is nil", ErrInvalidInput)
	}
	if builder.probe {
		return nil
	}
	canonical, err := materialScalarValue(value)
	if err != nil {
		return err
	}
	return builder.appendBytes(canonical.raw)
}

func materializePrepared(sources []preparedSource, components []int) (Materialization, error) {
	nodes := make([]materialNode, len(sources))
	for sourceID := range sources {
		nodes[sourceID] = materialNode{sourceID: sourceID, value: sources[sourceID].document}
	}

	// Probe the complete typed decision tree without encoding bytes or applying
	// the output budget. This makes ambiguity authoritative even when an earlier
	// deterministic branch would make a complete document exceed 4 MiB.
	probe := &materialBuilder{probe: true}
	ambiguous, err := materializeNode(probe, nodes, components)
	if err != nil {
		return Materialization{}, err
	}
	if ambiguous {
		return Materialization{Status: MaterializationStatusAmbiguous}, nil
	}

	// Only a fully decided tree is encoded, with the independent 4 MiB bound.
	builder := &materialBuilder{}
	ambiguous, err = materializeNode(builder, nodes, components)
	if err != nil {
		return Materialization{}, err
	}
	if ambiguous {
		return Materialization{Status: MaterializationStatusAmbiguous}, nil
	}
	return Materialization{
		Status: MaterializationStatusComplete,
		Data:   append(json.RawMessage(nil), builder.data...),
	}, nil
}

func materializeNode(builder *materialBuilder, nodes []materialNode, components []int) (bool, error) {
	kind, kindNodes, tied, err := winningNodeKind(nodes, components)
	if err != nil || tied {
		return tied, err
	}
	switch kind {
	case materialNodeObject:
		return materializeObject(builder, kindNodes, components)
	case materialNodeArray:
		return materializeArray(builder, kindNodes, components)
	case materialNodeScalar:
		return materializeScalar(builder, kindNodes, components)
	default:
		return false, fmt.Errorf("%w: unsupported materialization node", ErrInvalidInput)
	}
}

func winningNodeKind(nodes []materialNode, components []int) (materialNodeKind, []materialNode, bool, error) {
	if len(nodes) == 0 {
		return 0, nil, false, fmt.Errorf("%w: materialization node has no sources", ErrInvalidInput)
	}
	groups := make(map[materialNodeKind][]materialNode, 3)
	for _, node := range nodes {
		kind, err := classifyMaterialNode(node.value)
		if err != nil {
			return 0, nil, false, err
		}
		groups[kind] = append(groups[kind], node)
	}
	var winner materialNodeKind
	var winnerNodes []materialNode
	var winnerScore Agreement
	haveWinner := false
	tied := false
	for _, kind := range []materialNodeKind{materialNodeObject, materialNodeArray, materialNodeScalar} {
		group := groups[kind]
		if len(group) == 0 {
			continue
		}
		score := materialNodeAgreement(group, components)
		comparison := compareAgreement(score, winnerScore)
		if !haveWinner || comparison > 0 {
			winner, winnerNodes, winnerScore = kind, group, score
			haveWinner, tied = true, false
		} else if comparison == 0 {
			tied = true
		}
	}
	return winner, winnerNodes, tied, nil
}

func classifyMaterialNode(value any) (materialNodeKind, error) {
	switch value.(type) {
	case map[string]any:
		return materialNodeObject, nil
	case []any:
		return materialNodeArray, nil
	case exactNumber, string, bool, nil:
		return materialNodeScalar, nil
	default:
		return 0, fmt.Errorf("%w: unsupported decoded JSON structure", ErrInvalidInput)
	}
}

func materializeObject(builder *materialBuilder, nodes []materialNode, components []int) (bool, error) {
	if err := builder.appendByte('{'); err != nil {
		return false, err
	}
	keySet := make(map[string]struct{})
	for _, node := range nodes {
		for key := range node.value.(map[string]any) {
			keySet[key] = struct{}{}
		}
	}
	keys := make([]string, 0, len(keySet))
	for key := range keySet {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	written := 0
	for _, key := range keys {
		present := make([]materialNode, 0, len(nodes))
		absent := make([]materialNode, 0, len(nodes))
		for _, node := range nodes {
			child, exists := node.value.(map[string]any)[key]
			if exists {
				present = append(present, materialNode{sourceID: node.sourceID, value: child})
			} else {
				absent = append(absent, node)
			}
		}
		presence := materialNodeAgreement(present, components)
		absence := materialNodeAgreement(absent, components)
		switch compareAgreement(presence, absence) {
		case -1:
			continue
		case 0:
			return true, nil
		}
		if written > 0 {
			if err := builder.appendByte(','); err != nil {
				return false, err
			}
		}
		if err := builder.appendJSONString(key); err != nil {
			return false, err
		}
		if err := builder.appendByte(':'); err != nil {
			return false, err
		}
		ambiguous, err := materializeNode(builder, present, components)
		if err != nil || ambiguous {
			return ambiguous, err
		}
		written++
	}
	return false, builder.appendByte('}')
}

func materializeArray(builder *materialBuilder, nodes []materialNode, components []int) (bool, error) {
	byLength := make(map[int][]materialNode)
	for _, node := range nodes {
		length := len(node.value.([]any))
		byLength[length] = append(byLength[length], node)
	}
	lengths := make([]int, 0, len(byLength))
	for length := range byLength {
		lengths = append(lengths, length)
	}
	sort.Ints(lengths)
	winnerLength := 0
	winnerScore := Agreement{}
	haveWinner := false
	tied := false
	for _, length := range lengths {
		score := materialNodeAgreement(byLength[length], components)
		comparison := compareAgreement(score, winnerScore)
		if !haveWinner || comparison > 0 {
			winnerLength, winnerScore = length, score
			haveWinner, tied = true, false
		} else if comparison == 0 {
			tied = true
		}
	}
	if tied {
		return true, nil
	}
	winnerNodes := byLength[winnerLength]
	if err := builder.appendByte('['); err != nil {
		return false, err
	}
	for index := 0; index < winnerLength; index++ {
		if index > 0 {
			if err := builder.appendByte(','); err != nil {
				return false, err
			}
		}
		children := make([]materialNode, 0, len(winnerNodes))
		for _, node := range winnerNodes {
			array := node.value.([]any)
			children = append(children, materialNode{sourceID: node.sourceID, value: array[index]})
		}
		ambiguous, err := materializeNode(builder, children, components)
		if err != nil || ambiguous {
			return ambiguous, err
		}
	}
	return false, builder.appendByte(']')
}

func materializeScalar(builder *materialBuilder, nodes []materialNode, components []int) (bool, error) {
	type scalarGroup struct {
		value any
		nodes []materialNode
	}
	type scalarKey struct {
		kind    scalarKind
		boolean bool
		number  string
		text    string
	}
	byValue := make(map[scalarKey]*scalarGroup)
	for _, node := range nodes {
		var key scalarKey
		switch value := node.value.(type) {
		case nil:
			key.kind = scalarNull
		case bool:
			key.kind, key.boolean = scalarBoolean, value
		case exactNumber:
			key.kind, key.number = scalarNumber, value.groupKey
		case string:
			key.kind, key.text = scalarString, value
		default:
			return false, fmt.Errorf("%w: materialized value is not a JSON scalar", ErrInvalidInput)
		}
		group := byValue[key]
		if group == nil {
			group = &scalarGroup{value: node.value}
			byValue[key] = group
		}
		group.nodes = append(group.nodes, node)
	}
	var winner *scalarGroup
	winnerScore := Agreement{}
	tied := false
	for _, group := range byValue {
		score := materialNodeAgreement(group.nodes, components)
		comparison := compareAgreement(score, winnerScore)
		if winner == nil || comparison > 0 {
			winner, winnerScore, tied = group, score, false
		} else if comparison == 0 {
			tied = true
		}
	}
	if winner == nil {
		return false, fmt.Errorf("%w: materialized scalar has no field consensus", ErrInvalidInput)
	}
	if tied {
		return true, nil
	}
	return false, builder.appendScalar(winner.value)
}

func materialScalarValue(value any) (scalarValue, error) {
	switch value := value.(type) {
	case nil:
		return scalarValue{kind: scalarNull, raw: json.RawMessage("null"), groupKey: "null"}, nil
	case bool:
		raw := json.RawMessage(strconv.FormatBool(value))
		return scalarValue{kind: scalarBoolean, raw: raw, boolean: value, groupKey: "bool:" + string(raw)}, nil
	case exactNumber:
		return scalarValue{
			kind: scalarNumber, raw: json.RawMessage(value.canonical), number: value.value,
			groupKey: "number:" + value.groupKey,
		}, nil
	case string:
		raw := canonicalJSONString(value)
		return scalarValue{kind: scalarString, raw: raw, text: value, groupKey: "string:" + string(raw)}, nil
	default:
		return scalarValue{}, fmt.Errorf("%w: materialized value is not a JSON scalar", ErrInvalidInput)
	}
}

func materialNodeAgreement(nodes []materialNode, components []int) Agreement {
	independent := make(map[int]struct{}, len(nodes))
	for _, node := range nodes {
		independent[components[node.sourceID]] = struct{}{}
	}
	return Agreement{Pages: len(nodes), IndependentRoots: len(independent)}
}

func compareAgreement(first, second Agreement) int {
	if first.IndependentRoots < second.IndependentRoots {
		return -1
	}
	if first.IndependentRoots > second.IndependentRoots {
		return 1
	}
	if first.Pages < second.Pages {
		return -1
	}
	if first.Pages > second.Pages {
		return 1
	}
	return 0
}
