import fcntl,os,sys,struct
p=sys.argv[1]
fd=os.open(p,os.O_RDWR)
# flock probe
try:
    fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB); print("flock: acquired (NOT held by other)"); fcntl.flock(fd,fcntl.LOCK_UN)
except OSError as e: print("flock: held by other:",e)
# POSIX lock probe via F_GETLK
import ctypes,ctypes.util
# macOS struct flock: off_t l_start; off_t l_len; pid_t l_pid; short l_type; short l_whence
fmt="qqihh"
req=struct.pack(fmt,0,0,0,fcntl.F_WRLCK,0)
try:
    r=fcntl.fcntl(fd,fcntl.F_GETLK,req)
    s,l,pid,t,w=struct.unpack(fmt,r)
    print("F_GETLK: l_type=%d (F_UNLCK=%d) pid=%d"%(t,fcntl.F_UNLCK,pid))
except OSError as e: print("F_GETLK err",e)
