*** Settings ***
Documentation     Declared update recommendations through the CLI; discovery performs no effects.
Library           Process
Library           OperatingSystem
Library           routinglib.py
Suite Setup       Prepare Routing Bundles
Suite Teardown    Remove Directory    ${ROUTING_ROOT}    recursive=${True}

*** Variables ***
${BINARY}         ${EXECDIR}/build/omnicli
${RESOURCE}       stackql_unstable_demo.routing.widgets

*** Test Cases ***
All Matching Routes Are Recommended In Deterministic Order
    ${result}=    Discover Update Routes    valid    ["name"]
    ${rows}=    Parse Recommendations    ${result}
    ${priorities}=    Evaluate    [r['priority'] for r in $rows]
    ${targets}=    Evaluate    [r['target_method'] for r in $rows]
    Should Be Equal    ${priorities}    ${{[100, 40, 20, 20]}}
    Should Be Equal    ${targets}    ${{['high', 'high', 'low', 'middle']}}
    FOR    ${row}    IN    @{rows}
        Should Be Equal    ${row}[resource]    ${RESOURCE}
        Should Be Equal    ${row}[target_resource]    ${RESOURCE}
        Should Not Be Empty    ${row}[source]
        Should Not Be Empty    ${row}[location]
    END

Whole Change Set Must Be Permitted
    ${result}=    Discover Update Routes    valid    ["name","tags"]
    ${rows}=    Parse Recommendations    ${result}
    ${targets}=    Evaluate    [r['target_method'] for r in $rows]
    Should Be Equal    ${targets}    ${{['high', 'low']}}

Missing Required Attribute Excludes The Route
    ${result}=    Discover Update Routes    valid    ["tags"]
    ${rows}=    Parse Recommendations    ${result}
    Length Should Be    ${rows}    1
    Should Be Equal    ${rows}[0][target_method]    low

Empty Changes Are A No Op
    ${result}=    Discover Update Routes    valid    []
    Should Be Equal As Integers    ${result.rc}    0
    Should Be Empty    ${result.stdout}
    Should Be Empty    ${result.stderr}

Unhandled Changes Do Not Produce Partial Recommendations
    ${result}=    Discover Update Routes    valid    ["name","unhandled"]
    Should Be Equal As Integers    ${result.rc}    0
    Should Be Empty    ${result.stdout}
    Should Be Empty    ${result.stderr}

Invalid Target Is An Explicit Error
    ${result}=    Discover Update Routes    missing    ["name"]
    Should Not Be Equal As Integers    ${result.rc}    0
    Should Be Empty    ${result.stdout}
    Should Contain    ${result.stderr}    unknown method

Required Attributes Must Be Permitted
    ${result}=    Discover Update Routes    invalid    ["name"]
    Should Not Be Equal As Integers    ${result.rc}    0
    Should Be Empty    ${result.stdout}
    Should Contain    ${result.stderr}    subset of permits

*** Keywords ***
Prepare Routing Bundles
    ${root}=    Evaluate    tempfile.mkdtemp(prefix='omni-routing-robot-')    modules=tempfile
    Set Suite Variable    ${ROUTING_ROOT}    ${root}
    Write Routing Bundle    ${ROUTING_ROOT}/valid
    Write Routing Bundle    ${ROUTING_ROOT}/missing    invalid_target=${True}
    Write Routing Bundle    ${ROUTING_ROOT}/invalid    invalid_attributes=${True}

Discover Update Routes
    [Arguments]    ${bundle}    ${changed}
    ${result}=    Run Process    ${BINARY}    doc-update-routes    ${ROUTING_ROOT}/${bundle}    ${RESOURCE}    ${changed}
    RETURN    ${result}

Parse Recommendations
    [Arguments]    ${result}
    Should Be Equal As Integers    ${result.rc}    0    ${result.stderr}
    Should Be Empty    ${result.stderr}
    ${rows}=    Evaluate    [json.loads(line) for line in $result.stdout.splitlines()]    modules=json
    RETURN    ${rows}
