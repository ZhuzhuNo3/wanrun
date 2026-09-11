package runsupervisor

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const maximumDiagnosticBytes = 1024

// TransferResult is the supervisor's final process outcome for one run transfer.
type TransferResult struct {
	Transfer transfernumber.Number
	ExitCode int
	Signal   int
}

// SourceSummary is the frozen source-scan evidence retained through supervisor cleanup.
type SourceSummary struct {
	FollowedSymlinks    uint64
	IgnoredSymlinks     uint64
	EmptyDirectories    uint64
	IgnoredSpecialFiles uint64
}

// Final is the single terminal result written after supervisor cleanup.
type Final struct {
	Cancelled     bool
	Reason        CancelReason
	InternalError string
	RunError      string
	CleanupError  string
	Source        *SourceSummary
	Transfers     []TransferResult
}

// Clone returns a complete detached copy of the Final value.
func (final Final) Clone() Final {
	final.Transfers = append([]TransferResult(nil), final.Transfers...)
	if final.Source != nil {
		summary := *final.Source
		final.Source = &summary
	}
	return final
}

func encodeFinal(final Final) ([]byte, error) {
	final.InternalError = normalizeDiagnostic(final.InternalError)
	final.RunError = normalizeDiagnostic(final.RunError)
	final.CleanupError = normalizeDiagnostic(final.CleanupError)
	var err error
	final, err = orderedFinal(final)
	if err != nil {
		return nil, err
	}
	internal := []byte(final.InternalError)
	runFailure := []byte(final.RunError)
	cleanupFailure := []byte(final.CleanupError)
	payload := make([]byte, 42+len(internal)+len(runFailure)+len(cleanupFailure)+7*len(final.Transfers))
	if final.Cancelled {
		payload[0] = 1
	}
	payload[1] = byte(final.Reason)
	binary.BigEndian.PutUint16(payload[2:4], uint16(len(internal)))
	binary.BigEndian.PutUint16(payload[4:6], uint16(len(runFailure)))
	binary.BigEndian.PutUint16(payload[6:8], uint16(len(cleanupFailure)))
	if final.Source != nil {
		payload[8] = 1
		binary.BigEndian.PutUint64(payload[9:17], final.Source.FollowedSymlinks)
		binary.BigEndian.PutUint64(payload[17:25], final.Source.IgnoredSymlinks)
		binary.BigEndian.PutUint64(payload[25:33], final.Source.EmptyDirectories)
		binary.BigEndian.PutUint64(payload[33:41], final.Source.IgnoredSpecialFiles)
	}
	offset := 41
	copy(payload[offset:], internal)
	offset += len(internal)
	copy(payload[offset:], runFailure)
	offset += len(runFailure)
	copy(payload[offset:], cleanupFailure)
	offset += len(cleanupFailure)
	payload[offset] = byte(len(final.Transfers))
	offset++
	for _, result := range final.Transfers {
		payload[offset] = result.Transfer.Value()
		binary.BigEndian.PutUint32(payload[offset+1:offset+5], uint32(int32(result.ExitCode)))
		binary.BigEndian.PutUint16(payload[offset+5:offset+7], uint16(result.Signal))
		offset += 7
	}
	return payload, nil
}

func decodeFinal(payload []byte) (Final, error) {
	if len(payload) < 42 {
		return Final{}, fmt.Errorf("supervisor final payload is truncated")
	}
	internalSize := int(binary.BigEndian.Uint16(payload[2:4]))
	runSize := int(binary.BigEndian.Uint16(payload[4:6]))
	cleanupSize := int(binary.BigEndian.Uint16(payload[6:8]))
	textSize := internalSize + runSize + cleanupSize
	if internalSize > maximumDiagnosticBytes || runSize > maximumDiagnosticBytes ||
		cleanupSize > maximumDiagnosticBytes || 42+textSize > len(payload) {
		return Final{}, fmt.Errorf("supervisor final diagnostic is invalid")
	}
	if payload[8] > 1 {
		return Final{}, fmt.Errorf("supervisor final source summary flag is invalid")
	}
	offset := 41 + textSize
	count := int(payload[offset])
	offset++
	if count > transfernumber.Maximum || len(payload) != offset+7*count {
		return Final{}, fmt.Errorf("supervisor final transfer set is invalid")
	}
	textOffset := 41
	final := Final{Cancelled: payload[0] == 1, Reason: CancelReason(payload[1]),
		InternalError: string(payload[textOffset : textOffset+internalSize]), Transfers: make([]TransferResult, count)}
	if payload[8] == 1 {
		final.Source = &SourceSummary{FollowedSymlinks: binary.BigEndian.Uint64(payload[9:17]),
			IgnoredSymlinks:     binary.BigEndian.Uint64(payload[17:25]),
			EmptyDirectories:    binary.BigEndian.Uint64(payload[25:33]),
			IgnoredSpecialFiles: binary.BigEndian.Uint64(payload[33:41])}
	}
	textOffset += internalSize
	final.RunError = string(payload[textOffset : textOffset+runSize])
	textOffset += runSize
	final.CleanupError = string(payload[textOffset : textOffset+cleanupSize])
	if payload[0] > 1 {
		return Final{}, fmt.Errorf("supervisor final flags are invalid")
	}
	for index := range final.Transfers {
		id, err := transfernumber.New(int(payload[offset]))
		if err != nil {
			return Final{}, fmt.Errorf("supervisor final transfer is invalid")
		}
		final.Transfers[index] = TransferResult{Transfer: id,
			ExitCode: int(int32(binary.BigEndian.Uint32(payload[offset+1 : offset+5]))),
			Signal:   int(binary.BigEndian.Uint16(payload[offset+5 : offset+7]))}
		offset += 7
	}
	if err := validateFinal(final); err != nil {
		return Final{}, err
	}
	sort.Slice(final.Transfers, func(left, right int) bool {
		return final.Transfers[left].Transfer.Value() < final.Transfers[right].Transfer.Value()
	})
	return final, nil
}

func orderedFinal(final Final) (Final, error) {
	if err := validateFinal(final); err != nil {
		return Final{}, err
	}
	final.Transfers = append([]TransferResult(nil), final.Transfers...)
	sort.Slice(final.Transfers, func(left, right int) bool {
		return final.Transfers[left].Transfer.Value() < final.Transfers[right].Transfer.Value()
	})
	return final, nil
}

func validateFinal(final Final) error {
	if final.Cancelled != final.Reason.validCancellation() {
		return fmt.Errorf("supervisor final cancellation is inconsistent")
	}
	if len(final.InternalError) > maximumDiagnosticBytes || !utf8.ValidString(final.InternalError) ||
		len(final.RunError) > maximumDiagnosticBytes || !utf8.ValidString(final.RunError) ||
		len(final.CleanupError) > maximumDiagnosticBytes || !utf8.ValidString(final.CleanupError) {
		return fmt.Errorf("supervisor final diagnostic is invalid")
	}
	if len(final.Transfers) > transfernumber.Maximum {
		return fmt.Errorf("supervisor final has too many transfers")
	}
	seen := make(map[transfernumber.Number]struct{}, len(final.Transfers))
	for _, result := range final.Transfers {
		if !validTransferResult(result) {
			return fmt.Errorf("supervisor final transfer result is invalid")
		}
		if _, duplicate := seen[result.Transfer]; duplicate {
			return fmt.Errorf("supervisor final contains duplicate transfers")
		}
		seen[result.Transfer] = struct{}{}
	}
	for expected := 1; expected <= len(final.Transfers); expected++ {
		number, _ := transfernumber.New(expected)
		if _, exists := seen[number]; !exists {
			return fmt.Errorf("supervisor final is missing transfer %d", expected)
		}
	}
	return nil
}

func validTransferResult(result TransferResult) bool {
	return result.Transfer.Value() != 0 && result.Signal >= 0 && result.Signal <= 255 &&
		(result.Signal == 0 && result.ExitCode >= 0 && result.ExitCode <= 255 ||
			result.Signal != 0 && result.ExitCode == -1)
}

func diagnosticFor(action string, err error) string {
	if err == nil {
		return ""
	}
	return action + ": " + err.Error()
}

func finalEncodingFallback(final Final, encodeErr error) Final {
	fallback := Final{Cancelled: final.Cancelled, Reason: final.Reason,
		InternalError: joinDiagnostic(final.InternalError, "encode supervisor final: "+encodeErr.Error()),
		RunError:      normalizeDiagnostic(final.RunError), CleanupError: normalizeDiagnostic(final.CleanupError),
		Source: final.Source}
	if fallback.Cancelled {
		if !fallback.Reason.validCancellation() {
			fallback.Reason = CancelInternal
		}
	} else {
		fallback.Reason = CancelNone
	}
	valid := make(map[transfernumber.Number]TransferResult, len(final.Transfers))
	for _, result := range final.Transfers {
		if len(valid) == transfernumber.Maximum || !validTransferResult(result) {
			continue
		}
		if _, duplicate := valid[result.Transfer]; duplicate {
			continue
		}
		valid[result.Transfer] = result
	}
	for expected := 1; expected <= transfernumber.Maximum; expected++ {
		number, _ := transfernumber.New(expected)
		result, exists := valid[number]
		if !exists {
			break
		}
		fallback.Transfers = append(fallback.Transfers, result)
	}
	return fallback
}

func joinDiagnostic(left, right string) string {
	left = strings.ToValidUTF8(left, "\ufffd")
	right = strings.ToValidUTF8(right, "\ufffd")
	if left == "" {
		return truncateDiagnostic(right, maximumDiagnosticBytes)
	}
	if right == "" {
		return truncateDiagnostic(left, maximumDiagnosticBytes)
	}
	const separator = "; "
	if len(left)+len(separator)+len(right) <= maximumDiagnosticBytes {
		return left + separator + right
	}
	budget := maximumDiagnosticBytes - len(separator)
	leftLimit, rightLimit := budget/2, budget-budget/2
	if len(left) < leftLimit {
		leftLimit, rightLimit = len(left), budget-len(left)
	} else if len(right) < rightLimit {
		rightLimit, leftLimit = len(right), budget-len(right)
	}
	return truncateDiagnostic(left, leftLimit) + separator + truncateDiagnostic(right, rightLimit)
}

func boundedDiagnostic(value string) string { return normalizeDiagnostic(value) }

func normalizeDiagnostic(value string) string {
	value = strings.ToValidUTF8(value, "\ufffd")
	return truncateDiagnostic(value, maximumDiagnosticBytes)
}

func truncateDiagnostic(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}
