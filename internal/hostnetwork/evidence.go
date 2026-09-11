package hostnetwork

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"slices"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const claimVersion = 6

type networkClaim struct {
	version                int
	runID                  runid.ID
	ownerToken             string
	protocol               uint8
	backend                string
	iptablesFrontend       iptablesFrontend
	firewallTable          string
	nftPostroutingPriority int32
	returnTable            int
	forwardChains          []nftForwardChain
	transfers              []transferAllocation
}

type claimDocument struct {
	Version                int                     `json:"version"`
	RunID                  string                  `json:"run_id"`
	OwnerToken             string                  `json:"owner_token"`
	Protocol               uint8                   `json:"protocol"`
	Backend                string                  `json:"backend"`
	IPTablesFrontend       string                  `json:"iptables_frontend,omitempty"`
	FirewallTable          string                  `json:"firewall_table,omitempty"`
	NFTPostroutingPriority *int32                  `json:"nft_postrouting_priority,omitempty"`
	ReturnTable            int                     `json:"return_table"`
	ForwardChains          []nftForwardChain       `json:"forward_chains,omitempty"`
	Transfers              []claimTransferDocument `json:"transfers"`
}

type claimTransferDocument struct {
	Number           int    `json:"number"`
	Source           string `json:"source"`
	ProviderName     string `json:"provider_name"`
	ProviderIndex    int    `json:"provider_index"`
	RouteTable       int    `json:"route_table"`
	Gateway          string `json:"gateway,omitempty"`
	Subnet           string `json:"subnet"`
	HostIP           string `json:"host_ip"`
	NamespaceIP      string `json:"namespace_ip"`
	OutboundPriority int    `json:"outbound_priority"`
	ReturnPriority   int    `json:"return_priority"`
	LinkOwnerID      string `json:"link_owner_id"`
	HostVeth         string `json:"host_veth"`
	HostMAC          string `json:"host_mac"`
	PeerVeth         string `json:"peer_veth"`
	PeerMAC          string `json:"peer_mac"`
}

type activationDocument struct {
	Version      int    `json:"version"`
	RunID        string `json:"run_id"`
	ClaimVersion int    `json:"claim_version"`
	ClaimDigest  string `json:"claim_digest"`
}

func claimFromAllocation(allocation networkAllocation) networkClaim {
	transfers := append([]transferAllocation(nil), allocation.transfers...)
	return networkClaim{
		version: claimVersion, runID: allocation.runID, ownerToken: allocation.ownerToken,
		protocol: allocation.protocol, backend: string(allocation.backend),
		iptablesFrontend: allocation.iptablesFrontend, firewallTable: allocation.firewallTable,
		nftPostroutingPriority: allocation.nftPostroutingPriority, returnTable: allocation.returnTable,
		forwardChains: append([]nftForwardChain(nil), allocation.forwardChains...), transfers: transfers,
	}
}

func encodeClaim(claim networkClaim) ([]byte, error) {
	if err := validateClaim(claim, claim.runID); err != nil {
		return nil, err
	}
	document := claimDocument{
		Version: claim.version, RunID: claim.runID.String(), OwnerToken: claim.ownerToken,
		Protocol: claim.protocol, Backend: claim.backend,
		IPTablesFrontend: string(claim.iptablesFrontend), FirewallTable: claim.firewallTable,
		ReturnTable:   claim.returnTable,
		ForwardChains: append([]nftForwardChain(nil), claim.forwardChains...),
	}
	if firewallKind(claim.backend) == firewallNFTables {
		priority := claim.nftPostroutingPriority
		document.NFTPostroutingPriority = &priority
	}
	for _, transfer := range claim.transfers {
		document.Transfers = append(document.Transfers, claimTransferToDocument(transfer))
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode network claim: %w", err)
	}
	return append(encoded, '\n'), nil
}

func claimTransferToDocument(transfer transferAllocation) claimTransferDocument {
	document := claimTransferDocument{
		Number: transfer.number, Source: transfer.source.String(), ProviderName: transfer.providerName,
		ProviderIndex: transfer.providerIndex, RouteTable: transfer.routeTable, Subnet: transfer.subnet.String(),
		HostIP: transfer.hostIP.String(), NamespaceIP: transfer.namespaceIP.String(),
		OutboundPriority: transfer.outboundPriority, ReturnPriority: transfer.returnPriority,
		LinkOwnerID: transfer.linkOwner.String(), HostVeth: transfer.hostVeth,
		HostMAC: transfer.hostMAC.String(), PeerVeth: transfer.peerVeth, PeerMAC: transfer.peerMAC.String(),
	}
	if transfer.hasGateway {
		document.Gateway = transfer.gateway.String()
	}
	return document
}

func decodeClaim(reader io.Reader, expected runid.ID) (networkClaim, error) {
	data, err := readEvidence(reader, "network claim")
	if err != nil {
		return networkClaim{}, err
	}
	var document claimDocument
	if err := decodeStrictJSON(data, &document, "network claim"); err != nil {
		return networkClaim{}, err
	}
	claim, err := documentToClaim(document)
	if err != nil {
		return networkClaim{}, err
	}
	if err := validateClaim(claim, expected); err != nil {
		return networkClaim{}, err
	}
	return claim, nil
}

func documentToClaim(document claimDocument) (networkClaim, error) {
	id, err := runid.Parse(document.RunID)
	if err != nil {
		return networkClaim{}, fmt.Errorf("network claim run ID is invalid")
	}
	if firewallKind(document.Backend) == firewallNFTables && document.NFTPostroutingPriority == nil {
		return networkClaim{}, fmt.Errorf("network claim nftables priority is missing")
	}
	if firewallKind(document.Backend) != firewallNFTables && document.NFTPostroutingPriority != nil {
		return networkClaim{}, fmt.Errorf("network claim has a cross-backend nftables priority")
	}
	claim := networkClaim{
		version: document.Version, runID: id, ownerToken: document.OwnerToken, protocol: document.Protocol,
		backend: document.Backend, iptablesFrontend: iptablesFrontend(document.IPTablesFrontend),
		firewallTable: document.FirewallTable,
		returnTable:   document.ReturnTable, forwardChains: append([]nftForwardChain(nil), document.ForwardChains...),
	}
	if document.NFTPostroutingPriority != nil {
		claim.nftPostroutingPriority = *document.NFTPostroutingPriority
	}
	for _, transferDocument := range document.Transfers {
		transfer, err := documentToClaimTransfer(transferDocument)
		if err != nil {
			return networkClaim{}, err
		}
		claim.transfers = append(claim.transfers, transfer)
	}
	return claim, nil
}

func documentToClaimTransfer(document claimTransferDocument) (transferAllocation, error) {
	number, err := transfernumber.New(document.Number)
	if err != nil {
		return transferAllocation{}, fmt.Errorf("network claim transfer number is invalid")
	}
	source, err := netip.ParseAddr(document.Source)
	if err != nil {
		return transferAllocation{}, fmt.Errorf("network claim source is invalid")
	}
	subnet, err := netip.ParsePrefix(document.Subnet)
	if err != nil {
		return transferAllocation{}, fmt.Errorf("network claim subnet is invalid")
	}
	hostIP, err := netip.ParseAddr(document.HostIP)
	if err != nil {
		return transferAllocation{}, fmt.Errorf("network claim host IP is invalid")
	}
	namespaceIP, err := netip.ParseAddr(document.NamespaceIP)
	if err != nil {
		return transferAllocation{}, fmt.Errorf("network claim namespace IP is invalid")
	}
	linkOwner, err := parseLinkOwnerID(document.LinkOwnerID)
	if err != nil {
		return transferAllocation{}, fmt.Errorf("network claim link owner ID is invalid")
	}
	hostMAC, err := parseLinkMAC(document.HostMAC)
	if err != nil {
		return transferAllocation{}, fmt.Errorf("network claim host MAC is invalid")
	}
	peerMAC, err := parseLinkMAC(document.PeerMAC)
	if err != nil {
		return transferAllocation{}, fmt.Errorf("network claim peer MAC is invalid")
	}
	transfer := transferAllocation{
		transfer: number, number: document.Number, source: source, providerName: document.ProviderName,
		providerIndex: document.ProviderIndex, routeTable: document.RouteTable, subnet: subnet,
		hostIP: hostIP, namespaceIP: namespaceIP, outboundPriority: document.OutboundPriority,
		returnPriority: document.ReturnPriority, linkOwner: linkOwner,
		hostVeth: document.HostVeth, hostMAC: hostMAC, peerVeth: document.PeerVeth, peerMAC: peerMAC,
	}
	if document.Gateway != "" {
		transfer.gateway, err = netip.ParseAddr(document.Gateway)
		transfer.hasGateway = err == nil
		if err != nil {
			return transferAllocation{}, fmt.Errorf("network claim gateway is invalid")
		}
	}
	return transfer, nil
}

func validateClaim(claim networkClaim, expected runid.ID) error {
	if claim.version != claimVersion || claim.runID != expected || len(claim.transfers) == 0 ||
		len(claim.transfers) > transfernumber.Maximum || claim.returnTable < firstReturnTable ||
		claim.returnTable > lastReturnTable || claim.protocol < firstClaimProtocol ||
		claim.protocol > lastClaimProtocol || !validOwnerToken(claim.ownerToken) {
		return fmt.Errorf("network claim header or identity is invalid")
	}
	seenNames := make(map[string]struct{})
	switch firewallKind(claim.backend) {
	case firewallNFTables:
		if claim.iptablesFrontend != "" || claim.firewallTable != firewallName(expected) {
			return fmt.Errorf("network claim nftables identity is invalid")
		}
		if err := validateTransferLanesNFTPostroutingPriority(claim.nftPostroutingPriority); err != nil {
			return fmt.Errorf("network claim nftables identity is invalid: %w", err)
		}
		if err := validateNFTForwardChains(claim.forwardChains, claim.firewallTable); err != nil {
			return err
		}
		seenNames[claim.firewallTable] = struct{}{}
	case firewallIPTables:
		if claim.firewallTable != "" || claim.nftPostroutingPriority != 0 ||
			len(claim.forwardChains) != 0 || !knownIPTablesFrontend(claim.iptablesFrontend) {
			return fmt.Errorf("network claim iptables identity is invalid")
		}
		if err := validateIPTablesProviderNames(claim.transfers); err != nil {
			return err
		}
	default:
		return fmt.Errorf("network claim firewall backend is invalid")
	}
	seenNumbers, seenPriorities := map[int]struct{}{}, map[int]struct{}{}
	seenSubnets := make([]netip.Prefix, 0, len(claim.transfers))
	for index, transfer := range claim.transfers {
		if transfer.number != index+1 {
			return fmt.Errorf("network claim is missing transfer %d", index+1)
		}
		if err := validateClaimTransfer(claim, transfer, seenNames, seenNumbers,
			seenPriorities, seenSubnets); err != nil {
			return err
		}
		seenNumbers[transfer.number] = struct{}{}
		seenPriorities[transfer.outboundPriority], seenPriorities[transfer.returnPriority] = struct{}{}, struct{}{}
		seenSubnets = append(seenSubnets, transfer.subnet)
	}
	return nil
}

func validateClaimTransfer(claim networkClaim, transfer transferAllocation, names map[string]struct{},
	numbers, priorities map[int]struct{}, subnets []netip.Prefix,
) error {
	expectedOwner, err := deriveLinkOwnerID(claim.ownerToken, transfer.transfer)
	if err != nil {
		return fmt.Errorf("network claim transfer %d has invalid link owner: %w", transfer.number, err)
	}
	expectedPeerMAC, err := derivePeerMAC(claim.ownerToken, transfer.transfer)
	if err != nil {
		return fmt.Errorf("network claim transfer %d has invalid peer MAC: %w", transfer.number, err)
	}
	if transfer.number < 1 || transfer.number > transfernumber.Maximum ||
		transfer.transfer.Value() != uint8(transfer.number) || !transfer.source.Is4() ||
		transfer.source.IsUnspecified() || !validProviderName(transfer.providerName) || transfer.providerIndex <= 0 ||
		transfer.routeTable <= 0 || transfer.routeTable == claim.returnTable ||
		transfer.outboundPriority < firstPriority || transfer.outboundPriority > lastPriority ||
		transfer.returnPriority < firstPriority || transfer.returnPriority > lastPriority ||
		transfer.outboundPriority == transfer.returnPriority || transfer.subnet.Bits() != 30 ||
		transfer.subnet != transfer.subnet.Masked() || !temporaryRange.Contains(transfer.subnet.Addr()) ||
		transfer.hasGateway && !transfer.gateway.Is4() || !transfer.hasGateway && transfer.gateway.IsValid() ||
		transfer.hostIP != transfer.subnet.Addr().Next() || transfer.namespaceIP != transfer.hostIP.Next() ||
		transfer.linkOwner != expectedOwner || transfer.hostVeth != expectedOwner.hostName() ||
		transfer.hostMAC != expectedOwner.hostMAC() ||
		transfer.peerVeth != derivedPeerName(claim.runID, transfer.number) ||
		transfer.peerMAC != expectedPeerMAC {
		return fmt.Errorf("network claim transfer %d has invalid facts", transfer.number)
	}
	if _, exists := numbers[transfer.number]; exists {
		return fmt.Errorf("network claim repeats transfer %d", transfer.number)
	}
	for _, priority := range []int{transfer.outboundPriority, transfer.returnPriority} {
		if _, exists := priorities[priority]; exists {
			return fmt.Errorf("network claim repeats priority %d", priority)
		}
	}
	for _, name := range []string{transfer.hostVeth, transfer.peerVeth} {
		if _, exists := names[name]; exists {
			return fmt.Errorf("network claim repeats object identity %q", name)
		}
		names[name] = struct{}{}
	}
	if slices.ContainsFunc(subnets, transfer.subnet.Overlaps) {
		return fmt.Errorf("network claim repeats temporary subnet")
	}
	return nil
}

func validOwnerToken(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && !bytes.Equal(decoded, make([]byte, 32))
}

func newOwnerToken() (string, error) {
	for {
		var value [32]byte
		if _, err := rand.Read(value[:]); err != nil {
			return "", fmt.Errorf("generate network owner token: %w", err)
		}
		if value != ([32]byte{}) {
			return hex.EncodeToString(value[:]), nil
		}
	}
}

func claimsEqual(left, right networkClaim) bool {
	leftBytes, leftErr := encodeClaim(left)
	rightBytes, rightErr := encodeClaim(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}

func encodeTrafficActivation(claim networkClaim) ([]byte, error) {
	digest, err := claimDigest(claim)
	if err != nil {
		return nil, err
	}
	document := activationDocument{
		Version: 1, RunID: claim.runID.String(), ClaimVersion: claim.version,
		ClaimDigest: hex.EncodeToString(digest[:]),
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode traffic activation: %w", err)
	}
	return append(encoded, '\n'), nil
}

func decodeTrafficActivation(reader io.Reader, claim networkClaim) error {
	data, err := readEvidence(reader, "traffic activation")
	if err != nil {
		return err
	}
	var document activationDocument
	if err := decodeStrictJSON(data, &document, "traffic activation"); err != nil {
		return err
	}
	digest, err := claimDigest(claim)
	if err != nil {
		return err
	}
	if document.Version != 1 || document.RunID != claim.runID.String() ||
		document.ClaimVersion != claim.version || document.ClaimDigest != hex.EncodeToString(digest[:]) {
		return fmt.Errorf("traffic activation does not match the network claim")
	}
	return nil
}

func claimDigest(claim networkClaim) ([sha256.Size]byte, error) {
	encoded, err := encodeClaim(claim)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

func readEvidence(reader io.Reader, name string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, 1<<20+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("%s exceeds size limit", name)
	}
	return data, nil
}

func decodeStrictJSON(data []byte, destination any, name string) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	return requireJSONEnd(decoder)
}
