"""Run the exported flow twice in ephemeral n8n against mock gateway; no live credentials."""
import json,os,pathlib,secrets,subprocess,tempfile,time,urllib.request
root=pathlib.Path(__file__).resolve().parents[1]
image='docker.n8n.io/n8nio/n8n@sha256:c34ee9107a42b837e25cec0c8a927ab90a45b776aa2ccbf15e5b9cda466fad2c'
def run(*args):return subprocess.check_output(args,text=True,stderr=subprocess.STDOUT).strip()
with tempfile.TemporaryDirectory(prefix='tiktok-gateway-n8n-') as directory:
 d=pathlib.Path(directory);d.chmod(0o755);token=secrets.token_hex(24);(d/'token').write_text(token);(d/'token').chmod(0o444)
 name='tiktok-gateway-n8n-'+secrets.token_hex(4);network=name+'-net';gateway=name+'-gateway';n8n=name+'-n8n'
 workflow=json.loads((root/'examples/n8n-workflow.json').read_text());workflow['id']='gateway-smoke-workflow'
 for n in workflow['nodes']:
  if n['name']=='Webhook':
   n['type']='n8n-nodes-base.set';n['typeVersion']=3.4;n.pop('webhookId',None)
   n['parameters']={'assignments':{'assignments':[{'id':'event','name':'body','type':'object','value':json.dumps({"schemaVersion":"1","type":"follower.created","eventId":"synthetic-duplicate-event","data":{"userId":"200"}})}]},'options':{}}
  elif n['type']=='n8n-nodes-base.wait':n['parameters']['amount']=1
  elif n['name']=='Configuration':
   for a in n['parameters']['assignments']['assignments']:
    if a['name']=='gatewayUrl':a['value']='http://gateway:8080'
    if a['name']=='stickerId':a['value']='1'
  elif n['type']=='n8n-nodes-base.httpRequest':n['credentials']={'httpHeaderAuth':{'id':'gateway-test-credential','name':'Synthetic gateway token'}}
 workflow['nodes'].append({'id':'manual','name':'Manual','type':'n8n-nodes-base.manualTrigger','typeVersion':1,'position':[-200,0],'parameters':{}})
 workflow['connections']['Manual']={'main':[[{'node':'Webhook','type':'main','index':0}]]}
 (d/'workflow.json').write_text(json.dumps(workflow));(d/'credentials.json').write_text(json.dumps([{'id':'gateway-test-credential','name':'Synthetic gateway token','type':'httpHeaderAuth','data':{'name':'Authorization','value':'Bearer '+token}}]))
 try:
  run('docker','network','create',network)
  run('docker','run','-d','--name',gateway,'--network',network,'--network-alias','gateway','--tmpfs','/data:uid=65532,gid=65532,mode=0700','-v',str(d)+':/fixture:ro','-p','127.0.0.1::8080','-e','TIKTOK_MODE=mock','-e','TIKTOK_ACCOUNT_ID=100','-e','API_TOKEN_FILE=/fixture/token','-e','POLL_SECONDS=1',os.environ.get('GATEWAY_TEST_IMAGE','tiktok-gateway:smoke'))
  url='http://'+run('docker','port',gateway,'8080/tcp')
  def request(path,method='GET'):
   req=urllib.request.Request(url+path,data=b'' if method=='POST' else None,headers={'Authorization':'Bearer '+token},method=method)
   with urllib.request.urlopen(req,timeout=5) as r:return json.load(r)
  for _ in range(40):
   try:
    request('/readyz');break
   except OSError:time.sleep(0.25)
  request('/v1/control/resume','POST')
  output=run('docker','run','--rm','--name',n8n,'--network',network,'--tmpfs','/home/node/.n8n:uid=1000,gid=1000','-v',str(d)+':/fixture:ro','-e','N8N_DIAGNOSTICS_ENABLED=false','-e','N8N_VERSION_NOTIFICATIONS_ENABLED=false','--entrypoint','/bin/sh',image,'-c','n8n import:credentials --input=/fixture/credentials.json && n8n import:workflow --input=/fixture/workflow.json && n8n execute --id=gateway-smoke-workflow && n8n execute --id=gateway-smoke-workflow')
  if request('/v1/status')['queue']!={'succeeded':2}:
   print(output[-4000:]);raise AssertionError('Expected exactly two successful operations after two n8n executions')
  print('PASS: exported flow imports and runs twice in n8n; exactly one text and one sticker operation')
 except subprocess.CalledProcessError as error:
  print(error.output[-6000:]);raise
 finally:
  subprocess.run(['docker','rm','-f',gateway,n8n],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
  subprocess.run(['docker','network','rm',network],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
