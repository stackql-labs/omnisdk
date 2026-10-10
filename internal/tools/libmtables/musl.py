#!/usr/bin/env python3
"""Generate pkg/sqlfn/libm_musl_tables.go from musl's libm data tables.

Usage: musl.py <musl src/math> > pkg/sqlfn/libm_musl_tables.go

The snapshot is musl 7ada6dde, the one modernc.org/libc transpiles for Linux: exp_data.c, log_data.c,
log2_data.c and pow_data.c. Each value is kept as the C writes it, scale factors included.
"""
import re,sys
def parse(path, sizes):
    s=open(path).read()
    s=re.sub(r'/\*.*?\*/','',s,flags=re.S)
    s=re.sub(r'//[^\n]*','',s)
    s=re.sub(r'#if EXP_USE_TOINT_NARROW.*?#else(.*?)#endif',r'\1',s,flags=re.S)
    s=re.sub(r'#[^\n]*','',s)
    s=re.sub(r'A\(([^,]+),([^,]+),([^)]+)\)',r'{\1, 0, \2, \3},',s)
    body=s[s.index('= {',s.index('const struct'))+3:s.rindex('};')]
    fields={}
    i=0
    while True:
        m=re.compile(r'\.(\w+)\s*=\s*').search(body,i)
        if not m: break
        j=m.end()
        if body[j]=='{':
            depth=0;k=j
            while True:
                if body[k]=='{':depth+=1
                elif body[k]=='}':
                    depth-=1
                    if depth==0:break
                k+=1
            val=body[j:k+1]; i=k+1
            flat=val.replace('{',' ').replace('}',' ')
            nums=[x.strip() for x in flat.split(',') if x.strip()]
            fields[m.group(1)]=nums
        else:
            k=body.index(',',j); fields[m.group(1)]=body[j:k].strip(); i=k+1
    return fields
def gonum(x,isint=False):
    return x
out=['// Code generated from musl libm data tables (snapshot 7ada6dde, the one modernc.org/libc','// transpiles for Linux). DO NOT EDIT.','','package sqlfn','']
d=parse(sys.argv[1]+'/exp_data.c',{})
out.append('const muslExpN = 128\n')
out.append('var muslExp = struct {\n\tinvln2N, shift, negln2hiN, negln2loN float64\n\tpoly [4]float64\n\ttab [256]uint64\n}{')
out.append('\tinvln2N: %s,'%d['invln2N'].replace('N','muslExpN'))
out.append('\tshift: %s,'%d['shift'])
out.append('\tnegln2hiN: %s,'%d['negln2hiN'])
out.append('\tnegln2loN: %s,'%d['negln2loN'])
out.append('\tpoly: [4]float64{%s},'%', '.join(d['poly']))
assert len(d['tab'])==256
out.append('\ttab: [256]uint64{\n\t\t'+',\n\t\t'.join(', '.join(d['tab'][k:k+4]) for k in range(0,256,4))+',\n\t},')
out.append('}\n')
for name,path,n,p1,p in [('muslLog','log_data.c',128,'poly1','poly'),('muslLog2','log2_data.c',64,'poly1','poly')]:
    d=parse(sys.argv[1]+'/'+path,{})
    hi,lo=('ln2hi','ln2lo') if name=='muslLog' else ('invln2hi','invln2lo')
    out.append('var %s = struct {\n\thi, lo float64\n\tpoly [%d]float64\n\tpoly1 [%d]float64\n\ttab [%d][2]float64 // invc, logc\n\ttab2 [%d][2]float64 // chi, clo\n}{'%(name,len(d[p]),len(d[p1]),n,n))
    out.append('\thi: %s,\n\tlo: %s,'%(d[hi],d[lo]))
    out.append('\tpoly: [%d]float64{%s},'%(len(d[p]),', '.join(d[p])))
    out.append('\tpoly1: [%d]float64{%s},'%(len(d[p1]),', '.join(d[p1])))
    for t in ('tab','tab2'):
        v=d[t]; assert len(v)==2*n,(t,len(v))
        out.append('\t%s: [%d][2]float64{\n\t\t'%(t,n)+'\n\t\t'.join('{%s, %s},'%(v[k],v[k+1]) for k in range(0,2*n,2))+'\n\t},')
    out.append('}\n')
d=parse(sys.argv[1]+'/pow_data.c',{})
v=d['tab']; assert len(v)==4*128,len(v)
out.append('var muslPowLog = struct {\n\tln2hi, ln2lo float64\n\tpoly [%d]float64\n\ttab [128][4]float64 // invc, pad, logc, logctail\n}{'%len(d['poly']))
out.append('\tln2hi: %s,\n\tln2lo: %s,'%(d['ln2hi'],d['ln2lo']))
out.append('\tpoly: [%d]float64{%s},'%(len(d['poly']),', '.join(d['poly'])))
out.append('\ttab: [128][4]float64{\n\t\t'+'\n\t\t'.join('{%s},'%', '.join(v[k:k+4]) for k in range(0,512,4))+'\n\t},')
out.append('}')
print('\n'.join(out))
