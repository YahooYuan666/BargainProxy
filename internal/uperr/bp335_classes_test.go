// BP-335: upstream protocol faults must not charge the account.
//
// Before this class, a non-JSON body, a task that failed after acceptance, and
// a nonsense HTTP status all landed in ClassInternal, whose pool branch feeds
// ConsecutiveFails — upstream garbage benched healthy accounts (Coding2API
// 31ce9b03, 2026-11-23).
package uperr

import "testing"

func TestProtocolIsNotRotatable(t *testing.T) {
	if Rotatable(ClassProtocol) {
		t.Fatal("ClassProtocol must NOT rotate: the next account talks to the same broken surface")
	}
}

func TestEmptyReplyIsRotatable(t *testing.T) {
	if !Rotatable(ClassEmptyReply) {
		t.Fatal("ClassEmptyReply MUST rotate: another account may answer fine")
	}
}

// Neither class is an account verdict, so neither may be mistaken for one.
func TestNewClassesAreDistinct(t *testing.T) {
	if ClassProtocol == ClassInternal || ClassEmptyReply == ClassInternal {
		t.Fatal("the new classes must be distinct from ClassInternal")
	}
	if ClassProtocol == ClassEmptyReply {
		t.Fatal("protocol and empty_reply are different verdicts")
	}
}
