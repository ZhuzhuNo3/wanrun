package hostnetwork

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

type nftForwardChain struct {
	Family string `json:"family"`
	Table  string `json:"table"`
	Name   string `json:"name"`
}

const (
	nftConntrackPriority      int32 = -200
	iptablesSourceNATPriority int32 = 100
)

type nftPostroutingNATChain struct {
	Family   string
	Table    string
	Name     string
	Priority int32
	HasRules bool
}

func (chain nftPostroutingNATChain) canonicalIPTablesPostroutingChain() bool {
	return chain.Family == "ip" && chain.Table == "nat" && chain.Name == "POSTROUTING" &&
		chain.Priority == iptablesSourceNATPriority
}

func validateNFTPostroutingNATChains(chains []nftPostroutingNATChain) error {
	identities := make(map[[3]string]struct{}, len(chains))
	for index, chain := range chains {
		if chain.Family != "ip" && chain.Family != "inet" || chain.Table == "" || chain.Name == "" ||
			strings.ContainsRune(chain.Table, '\x00') || strings.ContainsRune(chain.Name, '\x00') {
			return fmt.Errorf("nftables postrouting NAT chain %d has incomplete identity", index)
		}
		if err := validateNFTNATPriority(chain.Priority); err != nil {
			return fmt.Errorf("nftables postrouting NAT chain %s/%s/%s: %w",
				chain.Family, chain.Table, chain.Name, err)
		}
		identity := [3]string{chain.Family, chain.Table, chain.Name}
		if _, duplicate := identities[identity]; duplicate {
			return fmt.Errorf("nftables postrouting NAT chain identity %s/%s/%s is duplicated",
				chain.Family, chain.Table, chain.Name)
		}
		identities[identity] = struct{}{}
	}
	return nil
}

func validateNFTNATPriority(priority int32) error {
	if priority <= nftConntrackPriority {
		return fmt.Errorf("NAT priority %d must be greater than conntrack priority %d",
			priority, nftConntrackPriority)
	}
	return nil
}

func validateTransferLanesNFTPostroutingPriority(priority int32) error {
	if err := validateNFTNATPriority(priority); err != nil {
		return err
	}
	if priority >= iptablesSourceNATPriority {
		return fmt.Errorf("NAT priority %d must precede standard source NAT priority %d",
			priority, iptablesSourceNATPriority)
	}
	return nil
}

func (chain nftForwardChain) canonicalIPTablesForwardChain() bool {
	return chain.Family == "ip" && chain.Table == "filter" && chain.Name == "FORWARD"
}

func validateNFTForwardChains(chains []nftForwardChain, ownedTable string) error {
	for index, chain := range chains {
		if chain.Family != "ip" && chain.Family != "inet" || chain.Table == "" || chain.Name == "" ||
			strings.ContainsRune(chain.Table, '\x00') || strings.ContainsRune(chain.Name, '\x00') ||
			chain.Family == "ip" && chain.Table == ownedTable {
			return fmt.Errorf("network claim nftables forward chain %d is invalid", index)
		}
		if index > 0 && !lessNFTForwardChain(chains[index-1], chain) {
			return fmt.Errorf("network claim nftables forward chains are not strictly ordered")
		}
	}
	return nil
}

func lessNFTForwardChain(left, right nftForwardChain) bool {
	return slices.Compare([]string{left.Family, left.Table, left.Name},
		[]string{right.Family, right.Table, right.Name}) < 0
}

type nftProgram struct {
	table               string
	ownerPrefix         string
	postroutingPriority int32
	chains              []string
	rules               []nftRule
	forwardRules        []nftForwardRule
}

type nftForwardRule struct {
	target nftForwardChain
	rule   nftRule
}

type nftRule struct {
	chain       string
	kind        string
	input       string
	output      string
	source      string
	destination string
	address     string
	owner       string
}

func buildNFTProgram(claim networkClaim) (nftProgram, error) {
	if err := validateClaim(claim, claim.runID); err != nil {
		return nftProgram{}, err
	}
	if firewallKind(claim.backend) != firewallNFTables {
		return nftProgram{}, fmt.Errorf("nftables program requires an nftables network claim")
	}
	ownerPrefix := "transferlanes:" + claim.runID.String() + ":" + claim.ownerToken + ":"
	program := nftProgram{table: claim.firewallTable, ownerPrefix: ownerPrefix,
		postroutingPriority: claim.nftPostroutingPriority,
		chains:              []string{"forward", "postrouting"}}
	for _, transfer := range claim.transfers {
		owner := fmt.Sprintf("%s%d", ownerPrefix, transfer.number)
		program.rules = append(program.rules,
			nftRule{chain: "forward", kind: "forward-out", input: transfer.hostVeth,
				output: transfer.providerName, source: transfer.subnet.String(), owner: owner + ":forward-out"},
			nftRule{chain: "forward", kind: "return", input: transfer.providerName,
				output: transfer.hostVeth, destination: transfer.subnet.String(), owner: owner + ":return"},
			nftRule{chain: "forward", kind: "drop-out", input: transfer.hostVeth, owner: owner + ":drop-out"},
			nftRule{chain: "forward", kind: "drop-in", output: transfer.hostVeth, owner: owner + ":drop-in"},
			nftRule{chain: "postrouting", kind: "snat", input: transfer.hostVeth,
				output: transfer.providerName, source: transfer.namespaceIP.String(),
				address: transfer.source.String(), owner: owner + ":snat"},
		)
	}
	for _, target := range claim.forwardChains {
		for _, transfer := range claim.transfers {
			owner := fmt.Sprintf("%shost-forward:%s:%d", ownerPrefix,
				nftForwardChainFingerprint(target), transfer.number)
			program.forwardRules = append(program.forwardRules,
				nftForwardRule{target: target, rule: nftRule{kind: "forward-out", input: transfer.hostVeth,
					output: transfer.providerName, source: transfer.subnet.String(), owner: owner + ":forward-out"}},
				nftForwardRule{target: target, rule: nftRule{kind: "return", input: transfer.providerName,
					output: transfer.hostVeth, destination: transfer.subnet.String(), owner: owner + ":return"}})
		}
	}
	return program, nil
}

func nftForwardChainFingerprint(chain nftForwardChain) string {
	digest := sha256.Sum256([]byte(chain.Family + "\x00" + chain.Table + "\x00" + chain.Name))
	return hex.EncodeToString(digest[:])
}
