*** Settings ***
Documentation     Caller identity and directory membership through the actual CLI; no cloud mocks.
Library           Process
Library           OperatingSystem
Library           accesslib.py
Suite Setup       Prepare Caller Configurations
Suite Teardown    Remove Directory    ${ACCESS_DIR}    recursive=${True}

*** Variables ***
${BINARY}         ${EXECDIR}/build/omnicli
${TOKEN}          robot-test-only-token

*** Test Cases ***
Bearer Whoami Returns Resolved Principal
    ${result}=    Run Caller Command    whoami    ${ACCESS_DIR}/bearer.json
    Should Succeed With JSON    ${result}    {"principal":"robot-user"}

Bearer Groups Returns Directory Memberships
    ${result}=    Run Caller Command    groups    ${ACCESS_DIR}/bearer.json
    Should Succeed With JSON    ${result}    {"groups":["readers","operators"]}

OS Whoami Returns Resolved Principal
    ${posix}=    Evaluate    hasattr(os, 'geteuid')    modules=os
    Skip If    not ${posix}    Effective UID authentication requires POSIX for this test.
    Write Caller Config    ${ACCESS_DIR}/os.json    mechanism=os
    ${result}=    Run Process    ${BINARY}    whoami    --access-config    ${ACCESS_DIR}/os.json
    Should Succeed With JSON    ${result}    {"principal":"robot-user"}

OS Groups Returns Directory Memberships
    ${posix}=    Evaluate    hasattr(os, 'geteuid')    modules=os
    Skip If    not ${posix}    Effective UID authentication requires POSIX for this test.
    Write Caller Config    ${ACCESS_DIR}/os.json    mechanism=os
    ${result}=    Run Process    ${BINARY}    groups    --access-config    ${ACCESS_DIR}/os.json
    Should Succeed With JSON    ${result}    {"groups":["readers","operators"]}

Empty Directory Membership Is An Empty Array
    ${result}=    Run Caller Command    groups    ${ACCESS_DIR}/empty.json
    Should Succeed With JSON    ${result}    {"groups":[]}

Missing Session Does Not Infer An Identity
    FOR    ${command}    IN    whoami    groups
        ${result}=    Run Process    ${BINARY}    ${command}
        Should Fail Without Identity Output    ${result}    no authenticated caller
    END

Invalid Credentials Do Not Reveal Identity Or Groups
    FOR    ${command}    IN    whoami    groups
        ${result}=    Run Caller Command    ${command}    ${ACCESS_DIR}/bearer.json    incorrect-test-token
        Should Fail Without Identity Output    ${result}    invalid bearer credential
        Should Not Contain    ${result.stderr}    incorrect-test-token
    END

Unresolved Identity Fails Closed
    ${result}=    Run Caller Command    whoami    ${ACCESS_DIR}/unknown.json
    Should Fail Without Identity Output    ${result}    identity is not in the directory

Missing Command Permission Fails Closed
    FOR    ${command}    IN    whoami    groups
        ${result}=    Run Caller Command    ${command}    ${ACCESS_DIR}/denied.json
        Should Fail Without Identity Output    ${result}    access denied
    END

*** Keywords ***
Prepare Caller Configurations
    ${directory}=    Evaluate    tempfile.mkdtemp(prefix='omni-access-robot-')    modules=tempfile
    Set Suite Variable    ${ACCESS_DIR}    ${directory}
    Write Caller Config    ${ACCESS_DIR}/bearer.json
    Write Caller Config    ${ACCESS_DIR}/empty.json    memberships=${False}
    Write Caller Config    ${ACCESS_DIR}/denied.json    permit=${False}
    Write Caller Config    ${ACCESS_DIR}/unknown.json    known_identity=${False}

Run Caller Command
    [Arguments]    ${command}    ${config}    ${token}=${TOKEN}
    ${result}=    Run Process    ${BINARY}    ${command}    --access-config    ${config}
    ...    --caller-token-env    OMNI_ROBOT_CALLER_TOKEN
    ...    env:OMNI_ROBOT_CALLER_TOKEN=${token}
    RETURN    ${result}

Should Succeed With JSON
    [Arguments]    ${result}    ${expected}
    Should Be Equal As Integers    ${result.rc}    0    ${result.stderr}
    Should Be Empty    ${result.stderr}
    ${actual}=    Evaluate    json.loads($result.stdout)    modules=json
    ${wanted}=    Evaluate    json.loads($expected)    modules=json
    Should Be Equal    ${actual}    ${wanted}
    Should Not Contain    ${result.stdout}    ${TOKEN}

Should Fail Without Identity Output
    [Arguments]    ${result}    ${message}
    Should Not Be Equal As Integers    ${result.rc}    0
    Should Be Empty    ${result.stdout}
    Should Contain    ${result.stderr}    ${message}
    Should Not Contain    ${result.stderr}    ${TOKEN}
