#!/usr/bin/env python3
"""Minimal UVC extension-unit (XU) access via UVCIOC_CTRL_QUERY.

  uvcxu.py DEV dump UNIT [UNIT...]            GET_INFO/LEN/CUR/MIN/MAX/DEF for selectors 1..32
  uvcxu.py DEV get UNIT SEL                    GET_CUR
  uvcxu.py DEV set UNIT SEL HEXBYTES           SET_CUR (runtime only, RAM; reset on replug)

Read-only queries are safe. Only use 'set' with documented runtime commands
(see docs/PIPELINES.md); some vendor XU commands write flash.
"""
import ctypes, fcntl, os, sys

class Q(ctypes.Structure):
    _fields_ = [("unit", ctypes.c_uint8), ("selector", ctypes.c_uint8),
                ("query", ctypes.c_uint16), ("size", ctypes.c_uint16),
                ("data", ctypes.c_void_p)]

UVCIOC_CTRL_QUERY = (3 << 30) | (ctypes.sizeof(Q) << 16) | (ord('u') << 8) | 0x21
SET_CUR, GET_CUR, GET_MIN, GET_MAX, GET_RES, GET_LEN, GET_INFO, GET_DEF = 0x01, 0x81, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87

def query(fd, unit, sel, q, size, payload=None):
    buf = (ctypes.c_uint8 * max(size, 1))()
    if payload:
        for i, b in enumerate(payload): buf[i] = b
    s = Q(unit, sel, q, size, ctypes.cast(buf, ctypes.c_void_p))
    fcntl.ioctl(fd, UVCIOC_CTRL_QUERY, s)
    return bytes(buf[:size])

def length(fd, unit, sel):
    return int.from_bytes(query(fd, unit, sel, GET_LEN, 2), "little")

def main():
    dev, cmd = sys.argv[1], sys.argv[2]
    fd = os.open(dev, os.O_RDWR)
    if cmd == "dump":
        for unit in map(int, sys.argv[3:]):
            for sel in range(1, 33):
                try:
                    info = query(fd, unit, sel, GET_INFO, 1)[0]
                    n = length(fd, unit, sel)
                except OSError:
                    continue
                line = f"unit {unit} sel {sel:2d} info=0x{info:02x} len={n}"
                for name, q in (("cur", GET_CUR), ("min", GET_MIN), ("max", GET_MAX), ("def", GET_DEF)):
                    try: line += f" {name}={query(fd, unit, sel, q, n).hex()}"
                    except OSError as e: line += f" {name}=E{e.errno}"
                print(line)
    elif cmd == "get":
        unit, sel = int(sys.argv[3]), int(sys.argv[4])
        print(query(fd, unit, sel, GET_CUR, length(fd, unit, sel)).hex())
    elif cmd == "set":
        unit, sel = int(sys.argv[3]), int(sys.argv[4])
        n = length(fd, unit, sel)
        data = bytes.fromhex(sys.argv[5]).ljust(n, b"\0")
        query(fd, unit, sel, SET_CUR, n, data)
    else:
        sys.exit(__doc__)

if __name__ == "__main__":
    main()
