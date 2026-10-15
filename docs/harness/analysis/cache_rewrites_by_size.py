import sqlite3,json,sys,collections
db=sqlite3.connect(sys.argv[1])
rows=db.execute("select session_id,output from traces where name like 'model:%' and output like '%usage%' order by session_id,started_at").fetchall()
prev={}; h=collections.Counter(); ht=collections.Counter(); allc=collections.Counter(); mods=collections.Counter()
for sid,o in rows:
    j=json.loads(o);u=j.get('usage',{}); m=j.get('model'); cw=u.get('cache_creation_tokens',0); tot=cw+u.get('cache_read_tokens',0)+u.get('input_tokens',0)
    b=min(tot//25000*25,250); allc[b]+=1
    p=prev.get(sid); prev[sid]=(m,tot)
    if not p or p[0]!=m or not tot or cw<5000 or cw/tot<=0.8: continue
    if 0.8<=tot/max(p[1],1)<1.3: h[b]+=1; ht[b]+=cw; mods[m]+=1
for b in sorted(allc): print(f"prompt {b:3d}k-{b+25}k: calls={allc[b]:5d} in-place rewrites={h[b]:4d} ({h[b]/allc[b]:.0%}) cw={ht[b]:>10,d}")
print(mods)
