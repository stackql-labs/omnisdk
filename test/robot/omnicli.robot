*** Settings ***
Library           Process
Library           OperatingSystem
Library           String
Library           mocklib.py
Suite Setup       Start Mock
Suite Teardown    Stop Mock

*** Variables ***
${PORT}           8085
${ENDPOINT}       http://127.0.0.1:${PORT}
${BINARY}         ${EXECDIR}/build/omnicli
${MOCKDIR}        ${CURDIR}/../mock
${OUTDIR}         ${CURDIR}/out
${EXPECTED}       ${MOCKDIR}/expected/encryption.jsonl
${OUT}            ${OUTDIR}/omnicli-encryption.jsonl
${PROVISION_JSON}    ${MOCKDIR}/collateral/ec2/provision.json
${PROVISION_OUT}     ${OUTDIR}/omnicli-provision.jsonl
${GCP_SA}            ${OUTDIR}/gcp-sa.json
${GCP_OUT}           ${OUTDIR}/omnicli-gcp.jsonl
${GCP_LOG}           ${OUTDIR}/omnicli-gcp-traffic.log
${AZURE_OUT}         ${OUTDIR}/omnicli-azure.jsonl
${AZURE_EXPECTED}    ${MOCKDIR}/expected/azure.jsonl
${BLOB_OUT}          ${OUTDIR}/omnicli-blob.jsonl
${GCP_ORG_OUT}       ${OUTDIR}/omnicli-blob-gcp-org.jsonl
${GCP_ORG_LOG}       ${OUTDIR}/omnicli-blob-gcp-org.log
${AZ_AUTH_OUT}       ${OUTDIR}/omnicli-azure-auth.jsonl
${OMNI_OUT}          ${OUTDIR}/omnicli-omni-composite.jsonl
${OMNI_MERGED_OUT}   ${OUTDIR}/omnicli-omni-merged.jsonl
${OMNI_ORG_OUT}      ${OUTDIR}/omnicli-omni-composite-org.jsonl
${OMNI_ORG_MERGED_OUT}    ${OUTDIR}/omnicli-omni-merged-org.jsonl
# Input columns echoed onto rows. The composite publishes its OWN three; the hand-assembled merge
# publishes the union of its legs' five. So equivalence is asserted on the provider data with these
# dropped — the echo differing is the signatures differing, not the query.
@{INPUT_COLS}        region    project    org    grpc_target    grpc_plaintext

*** Test Cases ***
Resources And Methods Discovery
    [Documentation]    Discovery off the facade catalog: list resources by regex, get a resource's
    ...    canonical schema, then list a resource's methods with their signatures (params + output
    ...    schema). Dot-path grammar. The CLI is a thin catalog consumer.
    ${list}=    Run Process    ${BINARY}    resources    --filter    storage
    ...    stdout=${OUTDIR}/resources.out    stderr=${OUTDIR}/resources.err
    Should Be Equal As Integers    ${list.rc}    0    omnicli failed: ${list.stderr}
    Should Contain    ${list.stdout}    azure.storage.accounts
    Should Contain    ${list.stdout}    google.storage.buckets
    ${res}=    Run Process    ${BINARY}    resources    google.storage.buckets
    ...    stdout=${OUTDIR}/resource.out    stderr=${OUTDIR}/resource.err
    Should Be Equal As Integers    ${res.rc}    0    omnicli failed: ${res.stderr}
    Should Contain    ${res.stdout}    "$schema"
    ${meth}=    Run Process    ${BINARY}    methods    google.storage.buckets
    ...    stdout=${OUTDIR}/methods.out    stderr=${OUTDIR}/methods.err
    Should Be Equal As Integers    ${meth.rc}    0    omnicli failed: ${meth.stderr}
    Should Contain    ${meth.stdout}    google.storage.buckets.list
    Should Contain    ${meth.stdout}    "name": "project"
    Should Contain    ${meth.stdout}    "encryption_class"
    # -q/--quiet is the bare, pipeable form: dot-paths only, nothing else
    ${methPaths}=    Run Process    ${BINARY}    methods    google.storage.buckets    --quiet
    ...    stdout=${OUTDIR}/methods-paths.out    stderr=${OUTDIR}/methods-paths.err
    Should Be Equal As Integers    ${methPaths.rc}    0    omnicli failed: ${methPaths.stderr}
    Should Be Equal    ${methPaths.stdout.strip()}    google.storage.buckets.list
    # ...and likewise for resources
    ${resPaths}=    Run Process    ${BINARY}    resources    -q
    ...    stdout=${OUTDIR}/resources-paths.out    stderr=${OUTDIR}/resources-paths.err
    Should Be Equal As Integers    ${resPaths.rc}    0    omnicli failed: ${resPaths.stderr}
    Should Contain    ${resPaths.stdout}    google.storage.buckets
    Should Not Contain    ${resPaths.stdout}    "summary"
    Should Not Contain    ${resPaths.stdout}    "$schema"
    ${one}=    Run Process    ${BINARY}    method    google.storage.buckets.list
    ...    stdout=${OUTDIR}/method.out    stderr=${OUTDIR}/method.err
    Should Be Equal As Integers    ${one.rc}    0    omnicli failed: ${one.stderr}
    Should Contain    ${one.stdout}    "path": "google.storage.buckets.list"
    Should Contain    ${one.stdout}    "$schema"

Generic Run Command Deserializes JSON Args
    [Documentation]    The generic `run <method> <json>` form: Args deserialized with Go's intrinsic
    ...    encoding/json. Same rows as the named command; tuning (limit) honored straight from the JSON.
    Write Gcp Service Account    ${GCP_SA}
    ${result}=    Run Process    ${BINARY}    run    google.storage.buckets.list    {"params":{"project":"mock-project"}}    --endpoint    ${ENDPOINT}    --out    ${GCP_OUT}
    ...    env:GOOGLE_APPLICATION_CREDENTIALS=${GCP_SA}
    ...    stdout=${OUTDIR}/run.out    stderr=${OUTDIR}/run.err
    Should Be Equal As Integers    ${result.rc}    0    omnicli failed: ${result.stderr}
    ${out}=    Get File    ${GCP_OUT}
    Should Contain    ${out}    "provider":"gcp"
    Should Contain    ${out}    gcs-cmek
    # tuning (limit) rides in the SAME JSON object — org scope capped to 1 row
    ${lim}=    Run Process    ${BINARY}    run    google.storage.buckets.list    {"params":{"org":"123456789"},"tuning":{"Limit":1}}    --endpoint    ${ENDPOINT}    --out    ${GCP_ORG_OUT}
    ...    env:GOOGLE_APPLICATION_CREDENTIALS=${GCP_SA}
    ...    stdout=${OUTDIR}/run-lim.out    stderr=${OUTDIR}/run-lim.err
    Should Be Equal As Integers    ${lim.rc}    0    omnicli failed: ${lim.stderr}
    ${limout}=    Get File    ${GCP_ORG_OUT}
    Should Contain X Times    ${limout}    "provider"    1

Auth Flows From The DTO
    [Documentation]    Credentials injected THROUGH the Auth DTO (inline values), with NO cred env vars
    ...    set — proving a consumer (stackql) can pass secrets per-request. Resolution is inline → env → file.
    ${result}=    Run Process    ${BINARY}    run    aws.s3.buckets.list    {"params":{"region":"us-east-1"},"auth":{"access_key_id":"AK","secret_access_key":"SK"}}    --endpoint    ${ENDPOINT}    --out    ${OUT}
    ...    env:AWS_ACCESS_KEY_ID=${EMPTY}    env:AWS_SECRET_ACCESS_KEY=${EMPTY}
    ...    stdout=${OUTDIR}/auth-dto.out    stderr=${OUTDIR}/auth-dto.err
    Should Be Equal As Integers    ${result.rc}    0    omnicli failed: ${result.stderr}
    ${out}=    Get File    ${OUT}
    Should Contain    ${out}    "provider":"aws"

Encryption Bowtie Matches Collateral
    [Documentation]    ListBuckets ⋈ GetBucketEncryption against the mock must reproduce the captured output exactly.
    ${result}=    Run Process    ${BINARY}    encryption    --endpoint    ${ENDPOINT}    --out    ${OUT}    --aws-region    us-east-1
    ...    env:AWS_ACCESS_KEY_ID=test    env:AWS_SECRET_ACCESS_KEY=test
    ...    stdout=${OUTDIR}/omnicli.out    stderr=${OUTDIR}/omnicli.err
    Should Be Equal As Integers    ${result.rc}    0    omnicli failed: ${result.stderr}
    Assert Jsonl Semantically Equal    ${OUT}    ${EXPECTED}

Provision Creates Vpc Then Subnet Via Beta
    [Documentation]    CreateVpc ⋈ CreateSubnet: ids match collateral; β proven because the mock 400s a
    ...    CreateSubnet whose VpcId ≠ the created VPC. Descriptions carry a runtime timestamp, so only their
    ...    format is asserted (exact match is impossible).
    ${result}=    Run Process    ${BINARY}    provision    --endpoint    ${ENDPOINT}    --out    ${PROVISION_OUT}
    ...    --vpc-cidr    10.0.0.0/16    --subnet-cidr    10.0.1.0/24    --aws-region    us-east-1
    ...    env:AWS_ACCESS_KEY_ID=test    env:AWS_SECRET_ACCESS_KEY=test
    ...    stdout=${OUTDIR}/omnicli-provision.out    stderr=${OUTDIR}/omnicli-provision.err
    Should Be Equal As Integers    ${result.rc}    0    omnicli failed: ${result.stderr}
    ${exp}=    Read Json    ${PROVISION_JSON}
    ${row}=    Read Json Line    ${PROVISION_OUT}
    Should Be Equal    ${row}[vpc_id]       ${exp}[vpc_id]
    Should Be Equal    ${row}[subnet_id]    ${exp}[subnet_id]
    Should Match Regexp    ${row}[vpc_description]       ^omnisdk vpc \\d{4}-\\d{2}-\\d{2}T
    Should Match Regexp    ${row}[subnet_description]    ^omnisdk subnet \\d{4}-\\d{2}-\\d{2}T

Provision Rejects Missing Required Cidr
    [Documentation]    A required κ input (vpc-cidr) absent → the plan is rejected instantly, before
    ...    any AWS call. Valid creds, no --vpc-cidr.
    ${result}=    Run Process    ${BINARY}    provision    --endpoint    ${ENDPOINT}    --subnet-cidr    10.0.1.0/24    --aws-region    us-east-1
    ...    env:AWS_ACCESS_KEY_ID=test    env:AWS_SECRET_ACCESS_KEY=test
    ...    stdout=${OUTDIR}/omnicli-prov-missing.out    stderr=${OUTDIR}/omnicli-prov-missing.err
    Should Not Be Equal As Integers    ${result.rc}    0
    Should Contain    ${result.stderr}    vpc_cidr

GCP Provision Chains OAuth Network Subnet
    [Documentation]    OAuth → CreateNetwork → poll → CreateSubnet → poll, against the GCP mock.
    ...    β(token) fans to every call (bearer); network/subnet ops are polled to DONE; the output
    ...    projects the returned ids + selfLinks (and never the token).
    Write Gcp Service Account    ${GCP_SA}
    ${result}=    Run Process    ${BINARY}    gcp-provision    --endpoint    ${ENDPOINT}    --project    mock-project    --out    ${GCP_OUT}    --log    ${GCP_LOG}
    ...    env:GOOGLE_APPLICATION_CREDENTIALS=${GCP_SA}
    ...    stdout=${OUTDIR}/gcp.out    stderr=${OUTDIR}/gcp.err
    Should Be Equal As Integers    ${result.rc}    0    omnicli failed: ${result.stderr}
    ${row}=    Read Json Line    ${GCP_OUT}
    Should Be Equal    ${row}[vpc_id]       1111
    Should Be Equal    ${row}[subnet_id]    2222
    Should Not Be Empty    ${row}[network_link]
    Should Not Be Empty    ${row}[subnet_link]
    Should Not Contain    ${result.stdout}    token
    # Every exchange has a β edge into the output: the output logs each one's output, redacted.
    ${log}=    Get File    ${GCP_LOG}
    Should Contain    ${log}    OAuth:
    Should Contain    ${log}    CreateNetwork:
    Should Contain    ${log}    PollNetwork:
    Should Contain    ${log}    PollSubnet:
    Should Not Contain    ${log}    mock-access-token

GCP Provision Rejects Missing Required Input
    [Documentation]    A required κ input (project) absent → the plan is rejected instantly, before
    ...    any network call. Valid creds, no --project.
    Write Gcp Service Account    ${GCP_SA}
    ${result}=    Run Process    ${BINARY}    gcp-provision    --endpoint    ${ENDPOINT}
    ...    env:GOOGLE_APPLICATION_CREDENTIALS=${GCP_SA}
    ...    stdout=${OUTDIR}/gcp-missing.out    stderr=${OUTDIR}/gcp-missing.err
    Should Not Be Equal As Integers    ${result.rc}    0
    Should Contain    ${result.stderr}    project

Azure Enumerates Subnets Across Org
    [Documentation]    Token → Subscriptions (paginated via nextLink) → VNets per sub → Subnets per VNet,
    ...    fanned out in parallel. Output must match the captured collateral (order-independent).
    ${result}=    Run Process    ${BINARY}    azure-vnets    --endpoint    ${ENDPOINT}    --out    ${AZURE_OUT}
    ...    env:AZURE_TENANT_ID=mock-tenant    env:AZURE_CLIENT_ID=mock-client    env:AZURE_CLIENT_SECRET=mock-secret
    ...    stdout=${OUTDIR}/azure.out    stderr=${OUTDIR}/azure.err
    Should Be Equal As Integers    ${result.rc}    0    omnicli failed: ${result.stderr}
    Assert Jsonl Semantically Equal    ${AZURE_OUT}    ${AZURE_EXPECTED}

Blob Audit Shallow Across Majors
    [Documentation]    Three disjoint DAGs (AWS S3, Azure storage accounts, GCP buckets) under
    ...    separate controllers, merged into one JSONL of {provider, name, encryption_status}.
    Write Gcp Service Account    ${GCP_SA}
    ${result}=    Run Process    ${BINARY}    blob-audit-shallow    --endpoint    ${ENDPOINT}    --out    ${BLOB_OUT}
    ...    --project    mock-project    --aws-region    us-east-1
    ...    env:AWS_ACCESS_KEY_ID=test    env:AWS_SECRET_ACCESS_KEY=test
    ...    env:AZURE_TENANT_ID=t    env:AZURE_CLIENT_ID=c    env:AZURE_CLIENT_SECRET=s
    ...    env:GOOGLE_APPLICATION_CREDENTIALS=${GCP_SA}
    ...    stdout=${OUTDIR}/blob.out    stderr=${OUTDIR}/blob.err
    Should Be Equal As Integers    ${result.rc}    0    omnicli failed: ${result.stderr}
    ${blob}=    Get File    ${BLOB_OUT}
    Should Contain    ${blob}    "provider":"aws"
    Should Contain    ${blob}    "provider":"azure"
    Should Contain    ${blob}    "provider":"gcp"
    Should Contain    ${blob}    "encryption_status":"Microsoft.Keyvault"
    Should Contain    ${blob}    gcs-cmek
    # normalized, consistent-across-clouds rendition
    Should Contain    ${blob}    "encryption_class":"customer-managed"
    Should Contain    ${blob}    "encryption_class":"provider-managed"
    Should Contain    ${blob}    "public":true
    Should Contain    ${blob}    "public":false
    Should Contain    ${blob}    "versioning":true
    Should Contain    ${blob}    "https":true

Blob Audit Shallow Org Across Majors
    [Documentation]    All-providers ORG-WIDE audit in one shot: AWS (account) + Azure (all subs) +
    ...    GCP (whole org, recursive via --gcp-org) as three disjoint DAGs. GCP contributes its full
    ...    org (4 rows: proj-root + proj-100, two buckets each); every provider present; no 400.
    Write Gcp Service Account    ${GCP_SA}
    ${result}=    Run Process    ${BINARY}    blob-audit-shallow-org    --endpoint    ${ENDPOINT}    --out    ${BLOB_OUT}    --log    ${GCP_ORG_LOG}
    ...    --gcp-org    123456789    --aws-region    us-east-1
    ...    env:AWS_ACCESS_KEY_ID=test    env:AWS_SECRET_ACCESS_KEY=test
    ...    env:AZURE_TENANT_ID=t    env:AZURE_CLIENT_ID=c    env:AZURE_CLIENT_SECRET=s
    ...    env:GOOGLE_APPLICATION_CREDENTIALS=${GCP_SA}
    ...    stdout=${OUTDIR}/blob-org.out    stderr=${OUTDIR}/blob-org.err
    Should Be Equal As Integers    ${result.rc}    0    omnicli failed: ${result.stderr}
    ${blob}=    Get File    ${BLOB_OUT}
    Should Contain    ${blob}    "provider":"aws"
    Should Contain    ${blob}    "provider":"azure"
    Should Contain X Times    ${blob}    "provider":"gcp"    4
    ${log}=    Get File    ${GCP_ORG_LOG}
    Should Not Contain    ${log}    → 400

Cross Cloud Composite Discovery And Signature
    [Documentation]    The cross-cloud audit is a FIRST-CLASS catalog entry: one resource
    ...    (omni.storage.buckets) carrying the uniform blob schema, and one composite method whose
    ...    signature is the union of its legs' scope inputs. A consumer discovers it exactly like a
    ...    single-provider resource — the member list is never its concern.
    ${list}=    Run Process    ${BINARY}    resources    --filter    ^omni
    ...    stdout=${OUTDIR}/omni-resources.out    stderr=${OUTDIR}/omni-resources.err
    Should Be Equal As Integers    ${list.rc}    0    omnicli failed: ${list.stderr}
    Should Contain    ${list.stdout}    omni.storage.buckets
    ${meth}=    Run Process    ${BINARY}    methods    omni.storage.buckets
    ...    stdout=${OUTDIR}/omni-methods.out    stderr=${OUTDIR}/omni-methods.err
    Should Be Equal As Integers    ${meth.rc}    0    omnicli failed: ${meth.stderr}
    Should Contain    ${meth.stdout}    omni.storage.buckets.list
    Should Contain    ${meth.stdout}    "name": "region"
    Should Contain    ${meth.stdout}    "name": "project"
    Should Contain    ${meth.stdout}    "name": "org"
    # it publishes the same uniform blob schema every leg normalizes to
    Should Contain    ${meth.stdout}    "encryption_class"
    Should Contain    ${meth.stdout}    "provider"
    # ...and the schema also carries the method's INPUTS as columns, marked as echoed input
    Should Contain    ${meth.stdout}    "x-omnisdk-input"

Method Response Schema Publishes Its Input Params
    [Documentation]    Signature and result contract are ONE: a method's declared inputs appear as
    ...    columns of its response schema (required input = plain type, optional = nullable), marked
    ...    x-omnisdk-input to separate them from provider data. Derived from Params, so they cannot
    ...    drift apart. Rows then carry those values, so every row states the scope that produced it.
    ${one}=    Run Process    ${BINARY}    method    omni.storage.buckets.list
    ...    stdout=${OUTDIR}/omni-signature.out    stderr=${OUTDIR}/omni-signature.err
    Should Be Equal As Integers    ${one.rc}    0    omnicli failed: ${one.stderr}
    ${sig}=    Evaluate    json.loads(r'''${one.stdout}''')    json
    ${props}=    Set Variable    ${sig}[schema][properties]
    # required input → plain string column; optional inputs → nullable
    Should Be Equal    ${props}[region][type]    string
    Should Be True    ${props}[region][x-omnisdk-input]
    Should Be Equal    ${props}[project][type]    ${{['string', 'null']}}
    Should Be Equal    ${props}[org][type]    ${{['string', 'null']}}
    # provider data columns are untouched by the echo
    Should Be Equal    ${props}[provider][type]    string
    Should Not Contain    ${props}[provider]    x-omnisdk-input
    # every input column is declared present on every row
    Should Contain    ${sig}[schema][required]    region
    Should Contain    ${sig}[schema][required]    project
    Should Contain    ${sig}[schema][required]    org

Rows Carry The Input Params That Produced Them
    [Documentation]    Each row states its own scope — which region/project/org produced it. For the
    ...    cross-cloud composite that is the ONLY way to attribute a row, since the legs are merged
    ...    into one cursor. An optional input that was not supplied is null, matching its schema.
    Write Gcp Service Account    ${GCP_SA}
    ${result}=    Run Process    ${BINARY}    run    omni.storage.buckets.list    {"params":{"region":"us-east-1","org":"123456789"}}
    ...    --endpoint    ${ENDPOINT}    --out    ${OMNI_OUT}
    ...    env:AWS_ACCESS_KEY_ID=test    env:AWS_SECRET_ACCESS_KEY=test
    ...    env:AZURE_TENANT_ID=t    env:AZURE_CLIENT_ID=c    env:AZURE_CLIENT_SECRET=s
    ...    env:GOOGLE_APPLICATION_CREDENTIALS=${GCP_SA}
    ...    stdout=${OUTDIR}/omni-echo.out    stderr=${OUTDIR}/omni-echo.err
    Should Be Equal As Integers    ${result.rc}    0    omnicli failed: ${result.stderr}
    ${rows}=    Get File    ${OMNI_OUT}
    ${lines}=    Split To Lines    ${rows}
    # EVERY row, from every leg, carries the scope — not just the leg the param belongs to
    FOR    ${line}    IN    @{lines}
        ${row}=    Evaluate    json.loads(r'''${line}''')    json
        Should Be Equal    ${row}[region]    us-east-1
        Should Be Equal    ${row}[org]    123456789
        Should Be Equal    ${row}[project]    ${None}    an unsupplied optional input must be null
    END
    # a method that declares NO params is untouched — no echoed columns appear
    ${az}=    Run Process    ${BINARY}    run    azure.storage.accounts.list    {}
    ...    --endpoint    ${ENDPOINT}    --out    ${AZ_AUTH_OUT}
    ...    env:AZURE_TENANT_ID=t    env:AZURE_CLIENT_ID=c    env:AZURE_CLIENT_SECRET=s
    ...    stdout=${OUTDIR}/omni-noparams.out    stderr=${OUTDIR}/omni-noparams.err
    Should Be Equal As Integers    ${az.rc}    0    omnicli failed: ${az.stderr}
    ${azRows}=    Get File    ${AZ_AUTH_OUT}
    Should Not Contain    ${azRows}    "region"

Cross Cloud Composite Enforces Its Own And Its Legs Params
    [Documentation]    Scope is required and never inferred. The composite enforces its OWN required
    ...    params up front, and each leg still validates its own when built — so the composite's
    ...    published signature cannot silently drift from its members.
    ${noRegion}=    Run Process    ${BINARY}    run    omni.storage.buckets.list    {"params":{"project":"mock-project"}}    --endpoint    ${ENDPOINT}
    ...    stdout=${OUTDIR}/omni-noregion.out    stderr=${OUTDIR}/omni-noregion.err
    Should Not Be Equal As Integers    ${noRegion.rc}    0    composite without region must fail
    Should Contain    ${noRegion.stderr}    requires param "region"
    ${noScope}=    Run Process    ${BINARY}    run    omni.storage.buckets.list    {"params":{"region":"us-east-1"}}    --endpoint    ${ENDPOINT}
    ...    env:AWS_ACCESS_KEY_ID=test    env:AWS_SECRET_ACCESS_KEY=test
    ...    env:AZURE_TENANT_ID=t    env:AZURE_CLIENT_ID=c    env:AZURE_CLIENT_SECRET=s
    ...    stdout=${OUTDIR}/omni-noscope.out    stderr=${OUTDIR}/omni-noscope.err
    Should Not Be Equal As Integers    ${noScope.rc}    0    composite without project/org must fail
    Should Contain    ${noScope.stderr}    omni.storage.buckets.list
    Should Contain    ${noScope.stderr}    google.storage.buckets.list

Cross Cloud Composite Equals The Merged Audit
    [Documentation]    The composite method and the hand-assembled merge are the SAME query: one
    ...    method path produces byte-for-byte the same row set as blob-audit-shallow's explicit
    ...    three-method NewMerged. Asserted for both project scope and org scope, so the composite is
    ...    proven to delegate scope through to its legs rather than reimplement them.
    Write Gcp Service Account    ${GCP_SA}
    # (a) project scope — the composite, selected as ONE method
    ${composite}=    Run Process    ${BINARY}    run    omni.storage.buckets.list    {"params":{"region":"us-east-1","project":"mock-project"}}
    ...    --endpoint    ${ENDPOINT}    --out    ${OMNI_OUT}
    ...    env:AWS_ACCESS_KEY_ID=test    env:AWS_SECRET_ACCESS_KEY=test
    ...    env:AZURE_TENANT_ID=t    env:AZURE_CLIENT_ID=c    env:AZURE_CLIENT_SECRET=s
    ...    env:GOOGLE_APPLICATION_CREDENTIALS=${GCP_SA}
    ...    stdout=${OUTDIR}/omni.out    stderr=${OUTDIR}/omni.err
    Should Be Equal As Integers    ${composite.rc}    0    omnicli failed: ${composite.stderr}
    # ...and the same audit assembled the old way, three method paths merged by the consumer
    ${merged}=    Run Process    ${BINARY}    blob-audit-shallow    --endpoint    ${ENDPOINT}    --out    ${OMNI_MERGED_OUT}
    ...    --project    mock-project    --aws-region    us-east-1
    ...    env:AWS_ACCESS_KEY_ID=test    env:AWS_SECRET_ACCESS_KEY=test
    ...    env:AZURE_TENANT_ID=t    env:AZURE_CLIENT_ID=c    env:AZURE_CLIENT_SECRET=s
    ...    env:GOOGLE_APPLICATION_CREDENTIALS=${GCP_SA}
    ...    stdout=${OUTDIR}/omni-merged.out    stderr=${OUTDIR}/omni-merged.err
    Should Be Equal As Integers    ${merged.rc}    0    omnicli failed: ${merged.stderr}
    Assert Jsonl Data Equal    ${OMNI_OUT}    ${OMNI_MERGED_OUT}    ${INPUT_COLS}
    # every provider really is in there (guards against an empty-equals-empty pass)
    ${omni}=    Get File    ${OMNI_OUT}
    Should Contain    ${omni}    "provider":"aws"
    Should Contain    ${omni}    "provider":"azure"
    Should Contain    ${omni}    "provider":"gcp"

    # (b) org scope — GCP leg descends the whole org; AWS/Azure unchanged
    ${compositeOrg}=    Run Process    ${BINARY}    run    omni.storage.buckets.list    {"params":{"region":"us-east-1","org":"123456789"}}
    ...    --endpoint    ${ENDPOINT}    --out    ${OMNI_ORG_OUT}
    ...    env:AWS_ACCESS_KEY_ID=test    env:AWS_SECRET_ACCESS_KEY=test
    ...    env:AZURE_TENANT_ID=t    env:AZURE_CLIENT_ID=c    env:AZURE_CLIENT_SECRET=s
    ...    env:GOOGLE_APPLICATION_CREDENTIALS=${GCP_SA}
    ...    stdout=${OUTDIR}/omni-org.out    stderr=${OUTDIR}/omni-org.err
    Should Be Equal As Integers    ${compositeOrg.rc}    0    omnicli failed: ${compositeOrg.stderr}
    ${mergedOrg}=    Run Process    ${BINARY}    blob-audit-shallow-org    --endpoint    ${ENDPOINT}    --out    ${OMNI_ORG_MERGED_OUT}
    ...    --gcp-org    123456789    --aws-region    us-east-1
    ...    env:AWS_ACCESS_KEY_ID=test    env:AWS_SECRET_ACCESS_KEY=test
    ...    env:AZURE_TENANT_ID=t    env:AZURE_CLIENT_ID=c    env:AZURE_CLIENT_SECRET=s
    ...    env:GOOGLE_APPLICATION_CREDENTIALS=${GCP_SA}
    ...    stdout=${OUTDIR}/omni-org-merged.out    stderr=${OUTDIR}/omni-org-merged.err
    Should Be Equal As Integers    ${mergedOrg.rc}    0    omnicli failed: ${mergedOrg.stderr}
    Assert Jsonl Data Equal    ${OMNI_ORG_OUT}    ${OMNI_ORG_MERGED_OUT}    ${INPUT_COLS}
    ${omniOrg}=    Get File    ${OMNI_ORG_OUT}
    Should Contain X Times    ${omniOrg}    "provider":"gcp"    4

Cross Cloud Composite Limit Caps The Union Globally
    [Documentation]    --limit is a GLOBAL result budget, not per-leg: the composite's three disjoint
    ...    DAGs sit under one output node, so N caps the union. Supplied in the same Args JSON.
    Write Gcp Service Account    ${GCP_SA}
    ${result}=    Run Process    ${BINARY}    run    omni.storage.buckets.list    {"params":{"region":"us-east-1","project":"mock-project"},"tuning":{"limit":3}}
    ...    --endpoint    ${ENDPOINT}    --out    ${OMNI_OUT}
    ...    env:AWS_ACCESS_KEY_ID=test    env:AWS_SECRET_ACCESS_KEY=test
    ...    env:AZURE_TENANT_ID=t    env:AZURE_CLIENT_ID=c    env:AZURE_CLIENT_SECRET=s
    ...    env:GOOGLE_APPLICATION_CREDENTIALS=${GCP_SA}
    ...    stdout=${OUTDIR}/omni-limit.out    stderr=${OUTDIR}/omni-limit.err
    Should Be Equal As Integers    ${result.rc}    0    omnicli failed: ${result.stderr}
    ${rows}=    Get File    ${OMNI_OUT}
    ${lines}=    Split To Lines    ${rows}
    Length Should Be    ${lines}    3    --limit must cap the UNION, not each leg

Azure Blob Audit Config Driven Client Credentials
    [Documentation]    Auth is switchable by JSON: type=client_credentials runs a token exchange
    ...    (Auth → {token}) then the same Subscriptions → StorageAccounts audit. Same result as the
    ...    hardcoded azure path, but selected by config.
    ${result}=    Run Process    ${BINARY}    blob-audit-shallow-azure-auth    --endpoint    ${ENDPOINT}    --out    ${AZ_AUTH_OUT}
    ...    --auth    {"type":"client_credentials","token_url":"${ENDPOINT}/mock-tenant/oauth2/v2.0/token","client_id_env_var":"AZURE_CLIENT_ID","client_secret_env_var":"AZURE_CLIENT_SECRET","scopes":["https://management.azure.com/.default"]}
    ...    env:AZURE_CLIENT_ID=c    env:AZURE_CLIENT_SECRET=s
    ...    stdout=${OUTDIR}/az-auth-cc.out    stderr=${OUTDIR}/az-auth-cc.err
    Should Be Equal As Integers    ${result.rc}    0    omnicli failed: ${result.stderr}
    ${blob}=    Get File    ${AZ_AUTH_OUT}
    Should Contain    ${blob}    "provider":"azure"
    Should Contain    ${blob}    "encryption_status":"Microsoft.Keyvault"
    Should Contain    ${blob}    "encryption_class":"customer-managed"

Azure Blob Audit Config Driven Bearer
    [Documentation]    Same audit, type=bearer (a pre-obtained OIDC token from env) — no token
    ...    exchange, the static {token} is injected as a κ input. Proves the auth swap is JSON-only.
    ${result}=    Run Process    ${BINARY}    blob-audit-shallow-azure-auth    --endpoint    ${ENDPOINT}    --out    ${AZ_AUTH_OUT}
    ...    --auth    {"type":"bearer","credentialsenvvar":"AZURE_OIDC_TOKEN"}
    ...    env:AZURE_OIDC_TOKEN=mock-azure-token
    ...    stdout=${OUTDIR}/az-auth-bearer.out    stderr=${OUTDIR}/az-auth-bearer.err
    Should Be Equal As Integers    ${result.rc}    0    omnicli failed: ${result.stderr}
    ${blob}=    Get File    ${AZ_AUTH_OUT}
    Should Contain    ${blob}    "provider":"azure"
    Should Contain    ${blob}    "encryption_class":"customer-managed"

Blob Audit Shallow GCP Org Descends Whole Hierarchy
    [Documentation]    Recursive folder descent (org → folders/100 → folders/200, depth ≥2) finds a
    ...    project at nodes that have one (proj-root, proj-100). folders/200 has NO project — that
    ...    empty scope must NOT leak a ?project= call (the real 400). Two projects × two buckets ⇒
    ...    four rows, and no 400 anywhere.
    Write Gcp Service Account    ${GCP_SA}
    ${result}=    Run Process    ${BINARY}    blob-audit-shallow-gcp-org    --endpoint    ${ENDPOINT}    --gcp-org    123456789    --out    ${GCP_ORG_OUT}    --log    ${GCP_ORG_LOG}
    ...    env:GOOGLE_APPLICATION_CREDENTIALS=${GCP_SA}
    ...    stdout=${OUTDIR}/blob-gcp-org.out    stderr=${OUTDIR}/blob-gcp-org.err
    Should Be Equal As Integers    ${result.rc}    0    omnicli failed: ${result.stderr}
    ${blob}=    Get File    ${GCP_ORG_OUT}
    Should Contain X Times    ${blob}    "provider":"gcp"    4
    # the empty-scope node (folders/200) must not have produced a bucket call → no 400 in the log
    ${log}=    Get File    ${GCP_ORG_LOG}
    Should Contain    ${log}    /v3/folders
    Should Contain    ${log}    /v3/projects
    Should Contain    ${log}    /storage/v1/b
    Should Contain    ${log}    → 200
    Should Not Contain    ${log}    → 400

Result Limit Stops Query Cleanly
    [Documentation]    --limit N terminates from the output side after exactly N records — a clean
    ...    stop (rc 0), not a crash or hang. The org audit would emit four gcp rows; --limit 2 → 2.
    Write Gcp Service Account    ${GCP_SA}
    ${result}=    Run Process    ${BINARY}    blob-audit-shallow-gcp-org    --endpoint    ${ENDPOINT}    --gcp-org    123456789    --limit    2    --out    ${GCP_ORG_OUT}
    ...    env:GOOGLE_APPLICATION_CREDENTIALS=${GCP_SA}
    ...    stdout=${OUTDIR}/limit.out    stderr=${OUTDIR}/limit.err
    Should Be Equal As Integers    ${result.rc}    0    omnicli failed: ${result.stderr}
    ${blob}=    Get File    ${GCP_ORG_OUT}
    Should Contain X Times    ${blob}    "provider":"gcp"    2

Result Limit Is Global Across Disconnected DAGs
    [Documentation]    --limit caps the WHOLE query, not each DAG: the multi-provider audit runs three
    ...    DISJOINT trees into one file, yet --limit 2 emits exactly 2 rows total and aborts the rest
    ...    cleanly — proving the output-triggered abort reaches disconnected nodes (the other sub-DAGs).
    Write Gcp Service Account    ${GCP_SA}
    ${result}=    Run Process    ${BINARY}    blob-audit-shallow-org    --endpoint    ${ENDPOINT}    --out    ${BLOB_OUT}    --limit    2
    ...    --gcp-org    123456789    --aws-region    us-east-1
    ...    env:AWS_ACCESS_KEY_ID=test    env:AWS_SECRET_ACCESS_KEY=test
    ...    env:AZURE_TENANT_ID=t    env:AZURE_CLIENT_ID=c    env:AZURE_CLIENT_SECRET=s
    ...    env:GOOGLE_APPLICATION_CREDENTIALS=${GCP_SA}
    ...    stdout=${OUTDIR}/limit-org.out    stderr=${OUTDIR}/limit-org.err
    Should Be Equal As Integers    ${result.rc}    0    omnicli failed: ${result.stderr}
    ${blob}=    Get File    ${BLOB_OUT}
    Should Contain X Times    ${blob}    "provider"    2

Blob Audit Shallow GCP Org 403 Lands In Log
    [Documentation]    Reproduces the real failure: org-level hierarchy listing denied (grants at
    ...    project, not org). The 403 must fail loudly AND be captured at the wire level in the traffic
    ...    log — it never reaches a decoded exchange output, which is why it was previously invisible.
    Write Gcp Service Account    ${GCP_SA}
    ${result}=    Run Process    ${BINARY}    blob-audit-shallow-gcp-org    --endpoint    ${ENDPOINT}    --gcp-org    000000000    --out    ${GCP_ORG_OUT}    --log    ${GCP_ORG_LOG}
    ...    env:GOOGLE_APPLICATION_CREDENTIALS=${GCP_SA}
    ...    stdout=${OUTDIR}/blob-gcp-403.out    stderr=${OUTDIR}/blob-gcp-403.err
    Should Not Be Equal As Integers    ${result.rc}    0
    Should Contain    ${result.stderr}    403
    # the error names the failing site, not just a bare status
    Should Contain    ${result.stderr}    /v3/folders
    ${log}=    Get File    ${GCP_ORG_LOG}
    Should Contain    ${log}    → 403
    Should Contain    ${log}    /v3/folders

Blob Audit Shallow GCP Requires Explicit Project
    [Documentation]    --project is compulsory user input (never inferred from env or the key's
    ...    project_id) — the single-project audit must fail loudly when it is absent.
    Write Gcp Service Account    ${GCP_SA}
    ${result}=    Run Process    ${BINARY}    blob-audit-shallow-gcp    --endpoint    ${ENDPOINT}
    ...    env:GOOGLE_APPLICATION_CREDENTIALS=${GCP_SA}
    ...    stdout=${OUTDIR}/blob-gcp-noproj.out    stderr=${OUTDIR}/blob-gcp-noproj.err
    Should Not Be Equal As Integers    ${result.rc}    0
    Should Contain    ${result.stderr}    project

Blob Audit Shallow GCP Org Requires Explicit Org
    [Documentation]    --gcp-org is compulsory scope input (never "whatever the SA can see") — the org
    ...    audit must fail loudly when it is absent.
    Write Gcp Service Account    ${GCP_SA}
    ${result}=    Run Process    ${BINARY}    blob-audit-shallow-gcp-org    --endpoint    ${ENDPOINT}
    ...    env:GOOGLE_APPLICATION_CREDENTIALS=${GCP_SA}
    ...    stdout=${OUTDIR}/blob-gcp-noorg.out    stderr=${OUTDIR}/blob-gcp-noorg.err
    Should Not Be Equal As Integers    ${result.rc}    0
    Should Contain    ${result.stderr}    gcp-org

*** Keywords ***
Start Mock
    Create Directory    ${OUTDIR}
    ${python}=    Python Executable
    ${proc}=    Start Process    ${python}    ${MOCKDIR}/app.py
    ...    env:PORT=${PORT}    stdout=${OUTDIR}/mock.out    stderr=${OUTDIR}/mock.err
    Set Suite Variable    ${MOCK}    ${proc}
    Wait For Server    ${ENDPOINT}/

Stop Mock
    Terminate Process    ${MOCK}
