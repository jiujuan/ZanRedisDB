package server

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"strconv"
	"strings"

	"github.com/absolute8511/redcon"
	"github.com/youzan/ZanRedisDB/common"
)

var (
	ErrUnknownCommand         = errors.New("unknown command")
	ErrWrongNumberOfArguments = errors.New("wrong number of arguments")
	ErrDisabled               = errors.New("disabled")
)

func GetIPv4ForInterfaceName(ifname string) string {
	inter, err := net.InterfaceByName(ifname)
	if err != nil {
		return ""
	}

	addrs, err := inter.Addrs()
	if err != nil {
		return ""
	}

	for _, addr := range addrs {
		if ip, ok := addr.(*net.IPNet); ok {
			if ip.IP.DefaultMask() != nil {
				return ip.IP.String()
			}
		}
	}

	return ""
}

// pipelineCommand creates a single command from a pipeline.
// should handle some pipeline command which across multi partitions
// since plget response is a bit complicated (order require), we do not handle pipeline for get
func pipelineCommand(conn redcon.Conn, cmd redcon.Command) (int, redcon.Command, error) {
	if conn == nil {
		return 0, cmd, nil
	}
	pcmds := conn.PeekPipeline()
	if len(pcmds) == 0 {
		return 0, cmd, nil
	}
	args := make([][]byte, 0, 64)
	switch qcmdlower(cmd.Args[0]) {
	default:
		return 0, cmd, nil
	case "plget", "plset":
		return 0, redcon.Command{}, ErrUnknownCommand
	case "set":
		if len(cmd.Args) != 3 {
			return 0, cmd, nil
		}
		// convert to a PLSET command which is similar to an MSET
		for _, pcmd := range pcmds {
			if qcmdlower(pcmd.Args[0]) != "set" || len(pcmd.Args) != 3 {
				return 0, cmd, nil
			}
		}
		args = append(args, []byte("plset"))
		for _, pcmd := range append([]redcon.Command{cmd}, pcmds...) {
			args = append(args, pcmd.Args[1], pcmd.Args[2])
		}
	}

	// remove the peeked items off the pipeline
	conn.ReadPipeline()

	ncmd := buildCommand(args)
	return len(pcmds) + 1, ncmd, nil
}

func buildCommand(args [][]byte) redcon.Command {
	return common.BuildCommand(args)
}

func parseCommand(raw []byte) (redcon.Command, error) {
	var cmd redcon.Command
	cmd.Raw = raw
	pos := 0
	rd := bufio.NewReader(bytes.NewBuffer(raw))
	c, err := rd.ReadByte()
	if err != nil {
		return cmd, err
	}
	pos++
	if c != '*' {
		return cmd, errors.New("invalid command")
	}
	line, err := rd.ReadString('\n')
	if err != nil {
		return cmd, err
	}
	pos += len(line)
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return cmd, errors.New("invalid command")
	}
	n, err := strconv.ParseUint(line[:len(line)-2], 10, 64)
	if err != nil {
		return cmd, err
	}
	if n == 0 {
		return cmd, errors.New("invalid command")
	}
	for i := uint64(0); i < n; i++ {
		c, err := rd.ReadByte()
		if err != nil {
			return cmd, err
		}
		pos++
		if c != '$' {
			return cmd, errors.New("invalid command")
		}
		line, err := rd.ReadString('\n')
		if err != nil {
			return cmd, err
		}
		pos += len(line)
		if len(line) < 2 || line[len(line)-2] != '\r' {
			return cmd, errors.New("invalid command")
		}
		n, err := strconv.ParseUint(line[:len(line)-2], 10, 64)
		if err != nil {
			return cmd, err
		}
		if _, err := rd.Discard(int(n) + 2); err != nil {
			return cmd, err
		}
		s := pos
		pos += int(n) + 2
		if raw[pos-2] != '\r' || raw[pos-1] != '\n' {
			return cmd, errors.New("invalid command")
		}
		cmd.Args = append(cmd.Args, raw[s:pos-2])
	}
	return cmd, nil
}

// qcmdlower for common optimized command lowercase conversions.
func qcmdlower(n []byte) string {
	switch len(n) {
	case 3:
		if (n[0] == 's' || n[0] == 'S') &&
			(n[1] == 'e' || n[1] == 'E') &&
			(n[2] == 't' || n[2] == 'T') {
			return "set"
		}
		if (n[0] == 'g' || n[0] == 'G') &&
			(n[1] == 'e' || n[1] == 'E') &&
			(n[2] == 't' || n[2] == 'T') {
			return "get"
		}
	case 4:
		if (n[0] == 'm' || n[0] == 'M') &&
			(n[1] == 's' || n[1] == 'S') &&
			(n[2] == 'e' || n[2] == 'E') &&
			(n[3] == 't' || n[3] == 'T') {
			return "mset"
		}
		if (n[0] == 'm' || n[0] == 'M') &&
			(n[1] == 'g' || n[1] == 'G') &&
			(n[2] == 'e' || n[2] == 'E') &&
			(n[3] == 't' || n[3] == 'T') {
			return "mget"
		}
		if (n[0] == 'e' || n[0] == 'E') &&
			(n[1] == 'v' || n[1] == 'V') &&
			(n[2] == 'a' || n[2] == 'A') &&
			(n[3] == 'l' || n[3] == 'L') {
			return "eval"
		}
	case 5:
		if (n[0] == 'p' || n[0] == 'P') &&
			(n[1] == 'l' || n[1] == 'L') &&
			(n[2] == 's' || n[2] == 'S') &&
			(n[3] == 'e' || n[3] == 'E') &&
			(n[4] == 't' || n[4] == 'T') {
			return "plset"
		}
		if (n[0] == 'p' || n[0] == 'P') &&
			(n[1] == 'l' || n[1] == 'L') &&
			(n[2] == 'g' || n[2] == 'G') &&
			(n[3] == 'e' || n[3] == 'E') &&
			(n[4] == 't' || n[4] == 'T') {
			return "plget"
		}
	case 6:
		if (n[0] == 'e' || n[0] == 'E') &&
			(n[1] == 'v' || n[1] == 'V') &&
			(n[2] == 'a' || n[2] == 'A') &&
			(n[3] == 'l' || n[3] == 'L') &&
			(n[4] == 'r' || n[4] == 'R') &&
			(n[5] == 'o' || n[5] == 'O') {
			return "evalro"
		}
	}
	return strings.ToLower(string(n))
}
