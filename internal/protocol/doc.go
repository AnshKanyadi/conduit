// Package protocol implements the Conduit binary framing protocol.
//
// Every byte sent over the wire between a Conduit client and server is
// wrapped in a Frame. This package owns serialization, deserialization,
// checksum validation, and sequence tracking.
//
// Wire format (see frame.go):
//
//	[ 4 bytes: magic 0xC0ND1337 ]
//	[ 1 byte:  frame type      ]
//	[ 2 bytes: sequence number ]
//	[ 4 bytes: session ID      ]
//	[ 2 bytes: payload length  ]
//	[ N bytes: payload         ]
//	[ 4 bytes: CRC32 checksum  ]
package protocol
