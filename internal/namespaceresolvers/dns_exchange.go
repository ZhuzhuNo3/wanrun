package namespaceresolvers

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

type resolverUpstream interface {
	exchange(context.Context, netip.Addr, []byte, bool, time.Duration) ([]byte, error)
	close() error
}

func (forwarder *dnsForwarder) answer(query []byte, oversized bool, transport string) ([]byte, error) {
	var resolveErr error
	var response []byte
	if oversized {
		resolveErr = &dnsMessageLimitError{transport: transport, direction: "request"}
	} else {
		response, resolveErr = forwarder.exchange(query)
	}
	if resolveErr == nil {
		return response, nil
	}
	forwarder.recordLimitFailure(resolveErr)
	response, responseErr := dnsFailureResponse(query)
	return response, responseErr
}

func (forwarder *dnsForwarder) exchange(query []byte) ([]byte, error) {
	if err := validateDNSMessage(query, false); err != nil {
		return nil, err
	}
	servers := forwarder.serverOrder()
	var failures []error
	for attempt := 0; attempt < forwarder.config.attempts; attempt++ {
		for _, server := range servers {
			response, err := forwarder.upstream.exchange(forwarder.context, server, query,
				forwarder.config.useTCP, forwarder.config.timeout)
			if isDNSMessageLimit(err) {
				return nil, err
			}
			if err == nil && !forwarder.config.useTCP && dnsTruncated(response) {
				response, err = forwarder.upstream.exchange(forwarder.context, server, query, true,
					forwarder.config.timeout)
				if isDNSMessageLimit(err) {
					return nil, err
				}
			}
			if err == nil {
				if validationErr := validateDNSResponse(query, response); validationErr == nil {
					return response, nil
				} else {
					err = validationErr
				}
			}
			failures = append(failures, err)
		}
	}
	return nil, errors.Join(failures...)
}

func (forwarder *dnsForwarder) serverOrder() []netip.Addr {
	servers := slices.Clone(forwarder.config.nameservers)
	if !forwarder.config.rotate || len(servers) < 2 {
		return servers
	}
	forwarder.rotateMu.Lock()
	offset := forwarder.next % len(servers)
	forwarder.next++
	forwarder.rotateMu.Unlock()
	return append(servers[offset:], servers[:offset]...)
}

func dnsFailureResponse(query []byte) ([]byte, error) {
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil || header.Response {
		return nil, errors.Join(errors.New("cannot identify failed DNS query"), err)
	}
	questions, err := parser.AllQuestions()
	if err != nil || len(questions) == 0 {
		return nil, errors.Join(errors.New("cannot identify failed DNS question"), err)
	}
	message := dnsmessage.Message{Header: dnsmessage.Header{ID: header.ID, Response: true,
		RecursionDesired: header.RecursionDesired, RecursionAvailable: true,
		RCode: dnsmessage.RCodeServerFailure}, Questions: questions}
	return message.Pack()
}

func validateDNSMessage(message []byte, response bool) error {
	if len(message) > maximumDNSMessage {
		return errors.New("DNS message exceeds the forwarding limit")
	}
	var decoded dnsmessage.Message
	err := decoded.Unpack(message)
	if err != nil || decoded.Response != response {
		return errors.Join(errors.New("DNS message is invalid"), err)
	}
	return nil
}

func validateDNSResponse(query, response []byte) error {
	if err := validateDNSMessage(query, false); err != nil {
		return err
	}
	if err := validateDNSMessage(response, true); err != nil {
		return err
	}
	var queryMessage, responseMessage dnsmessage.Message
	if err := queryMessage.Unpack(query); err != nil {
		return err
	}
	if err := responseMessage.Unpack(response); err != nil {
		return err
	}
	if queryMessage.ID != responseMessage.ID ||
		!slices.Equal(queryMessage.Questions, responseMessage.Questions) {
		return errors.New("DNS response identity changed")
	}
	return nil
}

func dnsTruncated(message []byte) bool { return len(message) >= 4 && message[2]&0x02 != 0 }
