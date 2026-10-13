"""Regenerate the credential-free n8n example deterministically."""
import json
from pathlib import Path
nodes=[]
connections={}
def node(name,kind,params,x,y=0,version=1):
    nodes.append(dict(id=name.lower().replace(' ','-'),name=name,type='n8n-nodes-base.'+kind,typeVersion=version,position=[x,y],parameters=params))
def link(source,target,output=0):
    main=connections.setdefault(source,{'main':[]})['main']
    while len(main)<=output: main.append([])
    main[output].append(dict(node=target,type='main',index=0))
def cond(name,expression,x,y=0):
    node(name,'if',{'conditions':{'options':{'caseSensitive':True,'leftValue':'','typeValidation':'strict','version':2},'conditions':[{'id':name,'leftValue':expression,'rightValue':True,'operator':{'type':'boolean','operation':'true','singleValue':True}}],'combinator':'and'},'options':{}},x,y,2.2)
node('Webhook','webhook',{'httpMethod':'POST','path':'tiktok-follower-gateway','authentication':'headerAuth','responseMode':'onReceived','options':{}},0,version=2)
nodes[-1]['webhookId']='tiktok-follower-gateway'
cond('Follower event',"={{ $json.body.schemaVersion === '1' && $json.body.type === 'follower.created' && typeof $json.body.eventId === 'string' && typeof $json.body.data.userId === 'string' }}",240)
node('Wait before welcome','wait',{'resume':'timeInterval','amount':60,'unit':'seconds'},480,version=1.1)
# All configurable values are non-secret. Immutable scenario/action IDs are literal in request keys.
node('Configuration','set',{'assignments':{'assignments':[{'id':'url','name':'gatewayUrl','value':'http://tiktok-gateway:8080','type':'string'},{'id':'sticker','name':'stickerId','value':'REPLACE_WITH_CATALOG_ID','type':'string'},{'id':'text','name':'text','value':'Привет! Спасибо за подписку!','type':'string'}]},'options':{}},720,version=3.4)
link('Webhook','Follower event');link('Follower event','Wait before welcome');link('Wait before welcome','Configuration')
for i,(label,kind) in enumerate([('Text','text'),('Sticker','sticker')]):
    x=960+i*1440
    body="={{ JSON.stringify({ recipientUserId: $('Webhook').first().json.body.data.userId, type: '"+kind+"', "+("text: $('Configuration').first().json.text" if kind=='text' else "stickerId: $('Configuration').first().json.stickerId")+" }) }}"
    auth={'authentication':'genericCredentialType','genericAuthType':'httpHeaderAuth'}
    node('Send '+label,'httpRequest',dict(auth,method='POST',url="={{ $('Configuration').first().json.gatewayUrl + '/v1/messages' }}",sendHeaders=True,headerParameters={'parameters':[{'name':'Idempotency-Key','value':"={{ $('Webhook').first().json.body.eventId + ':welcome-v1:"+kind+"-1' }}"}]},sendBody=True,contentType='raw',rawContentType='application/json',body=body,options={'timeout':30000}),x,version=4.2)
    node('Wait '+label,'wait',{'resume':'timeInterval','amount':5,'unit':'seconds'},x+220,version=1.1)
    node('Check '+label,'httpRequest',dict(auth,url="={{ $('Configuration').first().json.gatewayUrl + '/v1/operations/' + $('Send "+label+"').first().json.operationId }}",options={'timeout':30000}),x+440,version=4.2)
    cond(label+' succeeded',"={{ $json.state === 'succeeded' }}",x+660)
    cond(label+' pending',"={{ ['queued','sending','verifying'].includes($json.state) }}",x+660,220)
    node(label+' failed','stopAndError',{'errorMessage':"={{ 'TikTok "+kind+" operation ' + $json.operationId + ': ' + $json.state + ' (' + ($json.reason || 'unknown state') + '). Inspect gateway; never retry with a new key.' }}"},x+880,280)
    link('Send '+label,'Wait '+label);link('Wait '+label,'Check '+label);link('Check '+label,label+' succeeded');link(label+' succeeded',label+' pending',1);link(label+' pending','Wait '+label);link(label+' pending',label+' failed',1)
link('Configuration','Send Text');link('Text succeeded','Send Sticker')
workflow={'name':'TikTok Gateway — welcome v1','nodes':nodes,'connections':connections,'active':False,'settings':{'executionOrder':'v1'},'pinData':{},'tags':[]}
Path('examples/n8n-workflow.json').write_text(json.dumps(workflow,ensure_ascii=False,indent=2)+'\n')
