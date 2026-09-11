package transferstart

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
)

const wireVersion = byte(5)

var wireMagic = [4]byte{'W', 'T', 'R', 'Q'}

const (
	automaticFlag = byte(1)
	pipesFlag     = byte(2)
	symlinkFlag   = byte(4)
	knownFlags    = automaticFlag | pipesFlag | symlinkFlag
	fixedBytes    = 4 + 1 + 1 + 2 + 2 + 8 + 1 + 1 + 2
)

// EncodedSize returns the exact canonical byte length without encoding the request.
func EncodedSize(request Request) (int, error) {
	return checkedSize(request)
}

// Encode serializes one canonical V5 transfer start request.
func Encode(request Request) ([]byte, error) {
	size, err := checkedSize(request)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, 0, size)
	payload = append(payload, wireMagic[:]...)
	flags := byte(0)
	if request.AutomaticWeight() {
		flags |= automaticFlag
	}
	if request.commandIO.IsPipes() {
		flags |= pipesFlag
	}
	if request.followSymlinks {
		flags |= symlinkFlag
	}
	payload = append(payload, wireVersion, flags)
	terminal, _ := request.commandIO.PTYSize()
	columns, rows := terminal.Values()
	payload = binary.BigEndian.AppendUint16(payload, columns)
	payload = binary.BigEndian.AppendUint16(payload, rows)
	duration := time.Duration(0)
	if request.AutomaticWeight() {
		duration = request.settings.Duration()
	}
	payload = binary.BigEndian.AppendUint64(payload, uint64(duration))
	payload = append(payload, byte(request.networkCount()), byte(len(request.dns)))
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(request.childArgv)))
	payload = appendText(payload, request.sourceLabel)
	endpoint := ""
	if request.AutomaticWeight() {
		endpoint = request.settings.Endpoint()
	}
	payload = appendText(payload, endpoint)
	for _, network := range request.manual {
		payload = appendText(payload, network.localIP.String())
		payload = binary.BigEndian.AppendUint64(payload, network.weight)
	}
	for _, localIP := range request.automatic {
		payload = appendText(payload, localIP.String())
	}
	for _, resolver := range request.dns {
		payload = appendText(payload, resolver.String())
	}
	for _, argument := range request.childArgv {
		payload = appendText(payload, argument)
	}
	if len(payload) != size {
		return nil, errors.New("transfer start encoded size mismatch")
	}
	return payload, nil
}

// Decode rejects every non-canonical or unsupported transfer start payload.
func Decode(payload []byte) (Request, error) {
	reader := wireReader{payload: payload}
	header, err := decodeHeader(&reader)
	if err != nil {
		return Request{}, err
	}
	if header.automatic {
		header.automaticNetworks, err = decodeAutomaticNetworks(&reader, header.networks)
	} else {
		header.manualNetworks, err = decodeManualNetworks(&reader, header.networks)
	}
	if err != nil {
		return Request{}, err
	}
	dns, err := decodeAddresses(&reader, header.dns, "DNS")
	if err != nil {
		return Request{}, err
	}
	argv, err := decodeTexts(&reader, header.argv)
	if err != nil {
		return Request{}, err
	}
	if reader.remaining() != 0 {
		return Request{}, errors.New("transfer start request has trailing bytes")
	}
	if header.automatic {
		settings, settingsErr := throughput.NewSettings(header.endpoint, header.duration)
		if settingsErr != nil {
			return Request{}, invalidField("measure settings", settingsErr)
		}
		return NewAutomaticRequest(header.source, header.automaticNetworks, settings, dns, argv,
			header.commandIO, header.followSymlinks)
	}
	if header.endpoint != "" || header.duration != 0 {
		return Request{}, errors.New("manual transfer start contains measure settings")
	}
	return NewManualRequest(header.source, header.manualNetworks, dns, argv,
		header.commandIO, header.followSymlinks)
}

type decodedHeader struct {
	source            string
	endpoint          string
	duration          time.Duration
	networks          int
	dns               int
	argv              int
	automatic         bool
	followSymlinks    bool
	commandIO         CommandIO
	manualNetworks    []ManualNetwork
	automaticNetworks []netip.Addr
}

func decodeHeader(reader *wireReader) (decodedHeader, error) {
	magic, err := reader.take(len(wireMagic))
	if err != nil || string(magic) != string(wireMagic[:]) {
		return decodedHeader{}, errors.New("transfer start request magic is invalid")
	}
	version, err := reader.byte()
	if err != nil || version != wireVersion {
		return decodedHeader{}, errors.New("transfer start request version is invalid")
	}
	flags, err := reader.byte()
	if err != nil || flags&^knownFlags != 0 {
		return decodedHeader{}, errors.New("transfer start request flags are invalid")
	}
	columns, columnsErr := reader.uint16()
	rows, rowsErr := reader.uint16()
	durationValue, durationErr := reader.uint64()
	networks, networksErr := reader.byte()
	dns, dnsErr := reader.byte()
	argv, argvErr := reader.uint16()
	if err := errors.Join(columnsErr, rowsErr, durationErr, networksErr, dnsErr, argvErr); err != nil {
		return decodedHeader{}, err
	}
	if durationValue > uint64((60*time.Second).Nanoseconds()) || networks == 0 || argv == 0 {
		return decodedHeader{}, errors.New("transfer start request counts or duration are invalid")
	}
	source, sourceErr := reader.text()
	endpoint, endpointErr := reader.text()
	if err := errors.Join(sourceErr, endpointErr); err != nil {
		return decodedHeader{}, err
	}
	commandIO := Pipes()
	if flags&pipesFlag == 0 {
		var ioErr error
		commandIO, ioErr = NewPTY(int(columns), int(rows))
		if ioErr != nil {
			return decodedHeader{}, invalidField("PTY size", ioErr)
		}
	} else if columns != 0 || rows != 0 {
		return decodedHeader{}, errors.New("pipes transfer start contains a terminal size")
	}
	return decodedHeader{source: source, endpoint: endpoint, duration: time.Duration(durationValue),
		networks: int(networks), dns: int(dns), argv: int(argv), automatic: flags&automaticFlag != 0,
		followSymlinks: flags&symlinkFlag != 0, commandIO: commandIO}, nil
}

func decodeManualNetworks(reader *wireReader, count int) ([]ManualNetwork, error) {
	result := make([]ManualNetwork, count)
	for index := range result {
		value, readErr := reader.text()
		address, parseErr := netip.ParseAddr(value)
		weight, weightErr := reader.uint64()
		if err := errors.Join(readErr, parseErr, weightErr); err != nil {
			return nil, fmt.Errorf("decode transfer start manual network: %w", err)
		}
		network, err := NewManualNetwork(address, weight)
		if err != nil {
			return nil, err
		}
		result[index] = network
	}
	return result, nil
}

func decodeAutomaticNetworks(reader *wireReader, count int) ([]netip.Addr, error) {
	return decodeAddresses(reader, count, "automatic network")
}

func decodeAddresses(reader *wireReader, count int, label string) ([]netip.Addr, error) {
	result := make([]netip.Addr, count)
	for index := range result {
		value, readErr := reader.text()
		address, parseErr := netip.ParseAddr(value)
		if err := errors.Join(readErr, parseErr); err != nil {
			return nil, fmt.Errorf("decode transfer start %s: %w", label, err)
		}
		result[index] = address
	}
	return result, nil
}

func decodeTexts(reader *wireReader, count int) ([]string, error) {
	result := make([]string, count)
	for index := range result {
		value, err := reader.text()
		if err != nil {
			return nil, err
		}
		result[index] = value
	}
	return result, nil
}

func checkedSize(request Request) (int, error) {
	if err := request.validate(); err != nil {
		return 0, err
	}
	size := fixedBytes
	texts := []string{request.sourceLabel, ""}
	if request.AutomaticWeight() {
		texts[1] = request.settings.Endpoint()
	}
	for _, text := range texts {
		size += 2 + len(text)
	}
	for _, network := range request.manual {
		size += 2 + len(network.localIP.String()) + 8
	}
	for _, localIP := range request.automatic {
		size += 2 + len(localIP.String())
	}
	for _, resolver := range request.dns {
		size += 2 + len(resolver.String())
	}
	for _, argument := range request.childArgv {
		size += 2 + len(argument)
	}
	return size, nil
}

func appendText(payload []byte, value string) []byte {
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(value)))
	return append(payload, value...)
}

type wireReader struct {
	payload []byte
	offset  int
}

func (reader *wireReader) take(size int) ([]byte, error) {
	if size < 0 || reader.offset > len(reader.payload)-size {
		return nil, errors.New("transfer start request is truncated")
	}
	value := reader.payload[reader.offset : reader.offset+size]
	reader.offset += size
	return value, nil
}

func (reader *wireReader) byte() (byte, error) {
	value, err := reader.take(1)
	if err != nil {
		return 0, err
	}
	return value[0], nil
}

func (reader *wireReader) uint16() (uint16, error) {
	value, err := reader.take(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(value), nil
}

func (reader *wireReader) uint64() (uint64, error) {
	value, err := reader.take(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(value), nil
}

func (reader *wireReader) text() (string, error) {
	size, err := reader.uint16()
	if err != nil {
		return "", errors.Join(errors.New("transfer start text length is invalid"), err)
	}
	value, err := reader.take(int(size))
	if err != nil || !validText(string(value), false) {
		return "", errors.Join(errors.New("transfer start text encoding is invalid"), err)
	}
	return string(value), nil
}

func (reader *wireReader) remaining() int { return len(reader.payload) - reader.offset }
