import sqlite3,json,sys,collections
# (input, cw5m, cw1h, cache_read, output, tier_above, tier_mult_rates)
R={'claude-opus-5-5':(4,5,8,0.2,20),'claude-sonnet-5-5':(2,2.5,4,0.1,10),'claude-sonnet-4-6':(3,3.75,6,0.3,15),
   'claude-fable-5-1':(10,12.5,20,0.25,50),'claude-haiku-5-5':(0.1,0.125,0.2,0.01,0.5),
   'gpt-6.1-sol':(2,2.5,2.5,0.1,10),'gpt-6-astra':(10,12.5,12.5,1,50)}
TIER={'claude-haiku-5-5':(100000,(0.5,0.625,1,0.05,2.5)),'gpt-6.1-sol':(272000,(4,5,5,0.2,15)),'gpt-6-astra':(272000,(20,25,25,2,75))}
db=sqlite3.connect(sys.argv[1])
by=collections.defaultdict(collections.Counter); unpriced=collections.Counter(); comp=collections.Counter()
for (o,) in db.execute("select output from traces where name like 'model:%' and output like '%usage%'"):
    j=json.loads(o); m=j.get('model'); u=j['usage']
    inp=u.get('input_tokens',0); cw=u.get('cache_creation_tokens',0); cw1=u.get('cache_creation_1h_tokens',0); cr=u.get('cache_read_tokens',0); out=u.get('output_tokens',0)
    if m not in R: unpriced[m]+=1; continue
    r=R[m]
    if m in TIER and inp+cw+cr>TIER[m][0]: r=TIER[m][1]
    parts={'input':inp*r[0],'cache_write_5m':(cw-cw1)*r[1],'cache_write_1h':cw1*r[2],'cache_read':cr*r[3],'output':out*r[4]}
    for k,v in parts.items(): by[m][k]+=v/1e6; comp[k]+=v/1e6
    by[m]['calls']+=1
T=sum(comp.values())
print(f"TOTAL ${T:,.2f}")
for k,v in comp.most_common(): print(f"  {k:15s} ${v:8,.2f} {v/T:5.1%}")
for m,c in sorted(by.items(),key=lambda x:-sum(v for k,v in x[1].items() if k!='calls')):
    s=sum(v for k,v in c.items() if k!='calls'); print(f"  {m:20s} calls={int(c['calls']):5d} ${s:8,.2f}  cw=${c['cache_write_5m']+c['cache_write_1h']:.2f}")
print("unpriced",dict(unpriced))
