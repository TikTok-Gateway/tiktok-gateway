"""Isolated Compose smoke test. No live config, cookies, TikTok requests or host services."""
import json, os, pathlib, secrets, subprocess, tempfile, time, urllib.request, urllib.error
root=pathlib.Path(__file__).resolve().parents[1]
def run(*args): return subprocess.check_output(args,text=True,stderr=subprocess.STDOUT).strip()
with tempfile.TemporaryDirectory(prefix='tiktok-gateway-smoke-') as directory:
    d=pathlib.Path(directory); d.chmod(0o755); token=secrets.token_hex(24); hook=secrets.token_hex(24)
    for name,value in [('api',token),('hook',hook),('events.json','[]')]:
        (d/name).write_text(value);(d/name).chmod(0o444)
    (d/'receiver.py').write_text('''from http.server import BaseHTTPRequestHandler,HTTPServer
import json,os
received=[]
class Handler(BaseHTTPRequestHandler):
 def log_message(self,*args): pass
 def do_GET(self):
  self.send_response(200);self.end_headers();self.wfile.write(json.dumps(received).encode())
 def do_POST(self):
  if self.headers.get('Authorization')!='Bearer '+open('/fixture/hook').read():
   self.send_response(401);self.end_headers();return
  received.append(json.loads(self.rfile.read(int(self.headers['Content-Length']))))
  self.send_response(503 if len(received)<3 else 204);self.end_headers()
HTTPServer(('0.0.0.0',8080),Handler).serve_forever()
''')
    spec={'services':{'tiktok-gateway':{'image':os.environ.get('GATEWAY_TEST_IMAGE','tiktok-gateway:smoke'),'environment':{'TIKTOK_MODE':'mock','TIKTOK_ACCOUNT_ID':'100','API_TOKEN_FILE':'/fixture/api','WEBHOOK_TOKEN_FILE':'/fixture/hook','WEBHOOK_URL':'http://receiver:8080/','MOCK_EVENTS_FILE':'/fixture/events.json','POLL_SECONDS':'1'},'volumes':['data:/data',str(d)+':/fixture:ro'],'ports':['127.0.0.1::8080'],'read_only':True,'cap_drop':['ALL'],'security_opt':['no-new-privileges:true']},'receiver':{'image':'python:3.13-alpine','command':['python','/fixture/receiver.py'],'volumes':[str(d)+':/fixture:ro'],'ports':['127.0.0.1::8080']}},'volumes':{'data':{}}}
    (d/'compose.json').write_text(json.dumps(spec));project='tiktok-gateway-smoke-'+secrets.token_hex(4)
    base=['docker','compose','-p',project,'-f',str(d/'compose.json')]
    def compose(*args):return run(*base,*args)
    def port(service):return 'http://'+compose('port',service,'8080')
    def request(url,path,method='GET',body=None,key=None):
        headers={'Authorization':'Bearer '+token,'Content-Type':'application/json'}
        if key:headers['Idempotency-Key']=key
        req=urllib.request.Request(url+path,data=json.dumps(body).encode() if body is not None else (b'' if method=='POST' else None),headers=headers,method=method)
        with urllib.request.urlopen(req,timeout=4) as r:return json.load(r)
    def until(fn,seconds=70):
        deadline=time.monotonic()+seconds
        while time.monotonic()<deadline:
            try:
                value=fn()
                if value:return value
            except (OSError,ValueError): pass
            time.sleep(0.5)
        raise AssertionError('Timed out waiting for isolated smoke assertion')
    try:
        compose('up','-d');url=port('tiktok-gateway');receiver=port('receiver')
        until(lambda:request(url,'/readyz'))
        assert request(url,'/v1/status')['paused']
        cmd={'recipientUserId':'200','type':'text','text':'synthetic smoke message'}
        first=request(url,'/v1/messages','POST',cmd,'event:welcome-v1:text-1')
        assert first['state']=='queued'
        compose('up','-d','--force-recreate','tiktok-gateway');url=port('tiktok-gateway');until(lambda:request(url,'/readyz'))
        assert request(url,'/v1/status')['paused']
        again=request(url,'/v1/messages','POST',cmd,'event:welcome-v1:text-1');assert again['operationId']==first['operationId']
        request(url,'/v1/control/resume','POST')
        until(lambda:request(url,'/v1/operations/'+first['operationId'])['state']=='succeeded')
        # A duplicate webhook execution uses the same fixed action keys.
        for _ in range(2):
            duplicate=request(url,'/v1/messages','POST',cmd,'event:welcome-v1:text-1');assert duplicate['operationId']==first['operationId']
            sticker=request(url,'/v1/messages','POST',{'recipientUserId':'200','type':'sticker','stickerId':'1'},'event:welcome-v1:sticker-1')
            until(lambda:request(url,'/v1/operations/'+sticker['operationId'])['state']=='succeeded')
        assert request(url,'/v1/status')['queue']=={'succeeded':2}
        request(url,'/v1/control/pause','POST')
        (d/'events.json').chmod(0o644);(d/'events.json').write_text(json.dumps([{'ID':'notice-1','UserID':'200','Username':'synthetic','Created':int(time.time())}]));(d/'events.json').chmod(0o444)
        received=until(lambda:(lambda rows:rows if len(rows)>=3 else None)(request(receiver,'/')))
        assert len({row['eventId'] for row in received})==1
        until(lambda:request(url,'/v1/status')['webhook']['pending']==0)
        assert request(url,'/v1/status')['paused']
        logs=compose('logs');assert token not in logs and hook not in logs and cmd['text'] not in logs
        compose('up','-d','--force-recreate','tiktok-gateway');url=port('tiktok-gateway');until(lambda:request(url,'/readyz'))
        assert request(url,'/v1/status')['paused']
        assert request(url,'/v1/operations/'+first['operationId'])['state']=='succeeded'
        assert request(url,'/v1/status')['webhook']['pending']==0
        print('PASS: Compose mock, idempotency, text + sticker, pause, webhook retries, recreation persistence, secret-free logs')
    finally:
        compose('down','-v','--remove-orphans')
