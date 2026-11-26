import sys,json
C=__SKEIN_STARTER_CLIENT__
q=[]
M=" Mint a new monitor with the same parameters."
R=" Run the same command again"
def o(r,e,x,a):
 q.append(1)
 sys.stdout.write(json.dumps({"event":e,"reason":r,"action":"Loader download: "+a},separators=(",",":"))+"\n")
 sys.stdout.flush()
 sys.exit(x)
m=(3,12,4) if C and sys.platform=="win32" else (3,6)
if sys.version_info<m:
 o("config_invalid","closed",1,"Python %s or newer is needed. Install it."%".".join(map(str,m))+R+".")
import os,signal,hashlib,urllib.request as u
class S(BaseException):
 pass
def h(*a):
 if not q:
  q.append(1)
  raise S()
class N(u.HTTPRedirectHandler):
 def redirect_request(self,*a):
  return None
try:
 signal.signal(signal.SIGTERM,h)
 signal.signal(signal.SIGINT,h)
 b=os.environ.get("SKEIN_PUBLIC_URL","")
 w=os.environ.get("SKEIN_LOADER_SHA256","")
 if not b or not w:
  o("misconfigured","closed",1,"the start command is broken."+M)
 d=None
 try:
  r=u.build_opener(N).open(b+"/agent/v1/monitor-loader",timeout=30)
  if r.getcode()==200:
   d=r.read()
  r.close()
 except S:
  raise
 except Exception:
  pass
 if d is None:
  o("server_unreachable","paused",1,"the Skein server did not serve the loader."+R+" later.")
 if hashlib.sha256(d).hexdigest()!=w:
  o("runner_mismatch","closed",1,"the server's loader is of another build."+M)
 g={"__name__":"__main__"}
 if C:
  g["_SKEIN_LOADER_MODE"]="client"
 exec(d,g)
except S:
 o("stopped","paused",0,"stopped by a signal."+R+".")
