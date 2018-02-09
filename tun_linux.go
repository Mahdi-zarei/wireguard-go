package main

/* Implementation of the TUN device interface for linux
 */

import (
	"encoding/binary"
	"errors"
	"fmt"
	"git.zx2c4.com/wireguard-go/internal/events"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"strings"
	"time"
	"unsafe"
)

// #include <string.h>
// #include <unistd.h>
// #include <net/if.h>
// #include <netinet/in.h>
// #include <linux/netlink.h>
// #include <linux/rtnetlink.h>
//
// /* Creates a netlink socket
//  * listening to the RTMGRP_LINK multicast group
//  */
//
// int bind_rtmgrp() {
//   int nl_sock = socket(AF_NETLINK, SOCK_RAW, NETLINK_ROUTE);
//   if (nl_sock < 0)
//     return -1;
//
//	 struct sockaddr_nl addr;
//   memset ((void *) &addr, 0, sizeof (addr));
//   addr.nl_family = AF_NETLINK;
//   addr.nl_pid = getpid ();
//   addr.nl_groups = RTMGRP_LINK | RTMGRP_IPV4_IFADDR | RTMGRP_IPV6_IFADDR;
//
//   if (bind(nl_sock, (struct sockaddr *) &addr, sizeof (addr)) < 0)
//     return -1;
//
//   return nl_sock;
// }
import "C"

const (
	CloneDevicePath = "/dev/net/tun"
	IFReqSize       = unix.IFNAMSIZ + 64
)

type NativeTun struct {
	fd     *os.File
	index  int32             // if index
	name   string            // name of interface
	errors chan error        // async error handling
	events chan events.Event // device related events
}

func (tun *NativeTun) File() *os.File {
	return tun.fd
}

func (tun *NativeTun) RoutineHackListener() {
	/* This is needed for the detection to work across network namespaces
	 * If you are reading this and know a better method, please get in touch.
	 */
	fd := int(tun.fd.Fd())
	for {
		_, err := unix.Write(fd, nil)
		switch err {
		case unix.EINVAL:
			tun.events <- events.NewEvent(TUNEventUp)
		case unix.EIO:
			tun.events <- events.NewEvent(TUNEventDown)
		default:
		}
		time.Sleep(time.Second / 10)
	}
}

func (tun *NativeTun) RoutineNetlinkListener() {

	sock := int(C.bind_rtmgrp())
	if sock < 0 {
		tun.errors <- errors.New("Failed to create netlink event listener")
		return
	}

	for msg := make([]byte, 1<<16); ; {

		msgn, _, _, _, err := unix.Recvmsg(sock, msg[:], nil, 0)
		if err != nil {
			tun.errors <- fmt.Errorf("Failed to receive netlink message: %s", err.Error())
			return
		}

		for remain := msg[:msgn]; len(remain) >= unix.SizeofNlMsghdr; {

			hdr := *(*unix.NlMsghdr)(unsafe.Pointer(&remain[0]))

			if int(hdr.Len) > len(remain) {
				break
			}

			switch hdr.Type {
			case unix.NLMSG_DONE:
				remain = []byte{}

			case unix.RTM_NEWLINK:
				info := *(*unix.IfInfomsg)(unsafe.Pointer(&remain[unix.SizeofNlMsghdr]))
				remain = remain[hdr.Len:]

				if info.Index != tun.index {
					// not our interface
					continue
				}

				if info.Flags&unix.IFF_RUNNING != 0 {
					tun.events <- events.NewEvent(TUNEventUp)
				}

				if info.Flags&unix.IFF_RUNNING == 0 {
					tun.events <- events.NewEvent(TUNEventDown)
				}

				tun.events <- events.NewEvent(TUNEventMTUUpdate)

			default:
				remain = remain[hdr.Len:]
			}
		}
	}
}

func (tun *NativeTun) isUp() (bool, error) {
	inter, err := net.InterfaceByName(tun.name)
	return inter.Flags&net.FlagUp != 0, err
}

func (tun *NativeTun) Name() string {
	return tun.name
}

func getDummySock() (int, error) {
	return unix.Socket(
		unix.AF_INET,
		unix.SOCK_DGRAM,
		0,
	)
}

func getIFIndex(name string) (int32, error) {
	fd, err := getDummySock()
	if err != nil {
		return 0, err
	}

	defer unix.Close(fd)

	var ifr [IFReqSize]byte
	copy(ifr[:], name)
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.SIOCGIFINDEX),
		uintptr(unsafe.Pointer(&ifr[0])),
	)

	if errno != 0 {
		return 0, errno
	}

	index := binary.LittleEndian.Uint32(ifr[unix.IFNAMSIZ:])
	return toInt32(index), nil
}

func (tun *NativeTun) setMTU(n int) error {

	// open datagram socket

	fd, err := unix.Socket(
		unix.AF_INET,
		unix.SOCK_DGRAM,
		0,
	)

	if err != nil {
		return err
	}

	defer unix.Close(fd)

	// do ioctl call

	var ifr [IFReqSize]byte
	copy(ifr[:], tun.name)
	binary.LittleEndian.PutUint32(ifr[16:20], uint32(n))
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.SIOCSIFMTU),
		uintptr(unsafe.Pointer(&ifr[0])),
	)

	if errno != 0 {
		return errors.New("Failed to set MTU of TUN device")
	}

	return nil
}

func (tun *NativeTun) MTU() (int, error) {

	// open datagram socket

	fd, err := unix.Socket(
		unix.AF_INET,
		unix.SOCK_DGRAM,
		0,
	)

	if err != nil {
		return 0, err
	}

	defer unix.Close(fd)

	// do ioctl call

	var ifr [IFReqSize]byte
	copy(ifr[:], tun.name)
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.SIOCGIFMTU),
		uintptr(unsafe.Pointer(&ifr[0])),
	)
	if errno != 0 {
		return 0, errors.New("Failed to get MTU of TUN device")
	}

	// convert result to signed 32-bit int

	val := binary.LittleEndian.Uint32(ifr[16:20])
	if val >= (1 << 31) {
		return int(toInt32(val)), nil
	}
	return int(val), nil
}

func (tun *NativeTun) Write(buff []byte, offset int) (int, error) {

	// reserve space for header

	buff = buff[offset-4:]

	// add packet information header

	buff[0] = 0x00
	buff[1] = 0x00

	if buff[4] == ipv6.Version<<4 {
		buff[2] = 0x86
		buff[3] = 0xdd
	} else {
		buff[2] = 0x08
		buff[3] = 0x00
	}

	// write

	return tun.fd.Write(buff)
}

func (tun *NativeTun) Read(buff []byte, offset int) (int, error) {
	select {
	case err := <-tun.errors:
		return 0, err
	default:
		buff := buff[offset-4:]
		n, err := tun.fd.Read(buff[:])
		if n < 4 {
			return 0, err
		}
		return n - 4, err
	}
}

func (tun *NativeTun) Events() chan events.Event {
	return tun.events
}

func (tun *NativeTun) Close() error {
	return nil
}

func CreateTUNFromFile(name string, fd *os.File) (TUNDevice, error) {
	device := &NativeTun{
		fd:     fd,
		name:   name,
		events: make(chan events.Event, 5),
		errors: make(chan error, 5),
	}

	// start event listener

	var err error
	device.index, err = getIFIndex(device.name)
	if err != nil {
		return nil, err
	}

	go device.RoutineNetlinkListener()
	go device.RoutineHackListener() // cross namespace

	// set default MTU

	return device, device.setMTU(DefaultMTU)
}

func CreateTUN(name string) (TUNDevice, error) {

	// open clone device

	fd, err := os.OpenFile(CloneDevicePath, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}

	// create new device

	var ifr [IFReqSize]byte
	var flags uint16 = unix.IFF_TUN // | unix.IFF_NO_PI
	nameBytes := []byte(name)
	if len(nameBytes) >= unix.IFNAMSIZ {
		return nil, errors.New("Interface name too long")
	}
	copy(ifr[:], nameBytes)
	binary.LittleEndian.PutUint16(ifr[16:], flags)

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd.Fd()),
		uintptr(unix.TUNSETIFF),
		uintptr(unsafe.Pointer(&ifr[0])),
	)
	if errno != 0 {
		return nil, errno
	}

	// read (new) name of interface

	newName := string(ifr[:])
	newName = newName[:strings.Index(newName, "\000")]
	device := &NativeTun{
		fd:     fd,
		name:   newName,
		events: make(chan events.Event, 5),
		errors: make(chan error, 5),
	}

	// start event listener

	device.index, err = getIFIndex(device.name)
	if err != nil {
		return nil, err
	}

	go device.RoutineNetlinkListener()
	go device.RoutineHackListener() // cross namespace

	// set default MTU

	return device, device.setMTU(DefaultMTU)
}
