#!/usr/bin/env python3
"""Prepare an isolated provider overlay for the subnet public-IP update example."""
import argparse
from pathlib import Path
import shutil
import re



NAME_METHOD = """      updates:
        - priority: 100
          requires: [DebugCorrelation]
          permits: [DebugCorrelation]
          target: {$ref: '#/components/x-stackQL-resources/vpcs/methods/debug_correlation'}
        - priority: 100
          requires: [Name]
          permits: [Name]
          target: {$ref: '#/components/x-stackQL-resources/vpcs/methods/set_name'}
      methods:
        set_name:
          operation: {$ref: '#/paths/~1?Action=CreateTags&Version=2016-11-15/post'}
          request:
            mediaType: application/x-www-form-urlencoded
            transform:
              type: golang_template_text_v0.3.0
              body: '{{ $d := jsonMapFromString . }}{{ if ne (kindOf (index $d "Name")) "string" }}{{ template "Name must be a string; use a string value to set the tag" . }}{{ end }}ResourceId.1={{ urlquery (index $d "VpcId") }}&Tag.1.Key=Name&Tag.1.Value={{ urlquery (index $d "Name") }}'
          response: {openAPIDocKey: '200'}
"""

NAME_OPERATION = """    post:
      operationId: POST_SetVpcName
      parameters:
        - {name: VpcId, in: body, required: true, schema: {type: string}}
      requestBody:
        content:
          application/x-www-form-urlencoded:
            schema:
              type: object
              properties:
                Name: {type: string}
      responses:
        '200': {description: Tag set}
"""

NAME_READ = '''{{ define "tag" }}{{ if eq (index . "key") "Name" }},"Name":{{ toJson (index . "value") }}{{ else if eq (index . "key") "omnisdk:key" }},"DebugCorrelation":{{ toJson (index . "value") }}{{ else if eq (index . "key") "resource-key" }},"TagSpecification.1.Tag.1.Value":{{ toJson (index . "value") }}{{ end }}{{ end }}
{{ define "vpc" }}{"VpcId":{{ toJson (index . "vpcId") }},"CidrBlock":{{ toJson (index . "cidrBlock") }},"InstanceTenancy":{{ toJson (index . "instanceTenancy") }}
{{ $tags := index . "tagSet" "item" }}{{ if eq (kindOf $tags) "slice" }}{{ range $tags }}{{ template "tag" . }}{{ end }}{{ else if eq (kindOf $tags) "map" }}{{ template "tag" $tags }}{{ end }}}
{{ end }}
{"line_items":[{{ $rows := index . "DescribeVpcsResponse" "vpcSet" "item" }}{{ if eq (kindOf $rows) "slice" }}{{ $sep := separator "," }}{{ range $rows }}{{ call $sep }}{{ template "vpc" . }}{{ end }}{{ else if eq (kindOf $rows) "map" }}{{ template "vpc" $rows }}{{ end }}]}'''


SUBNET_READ = NAME_READ.replace('DescribeVpcsResponse', 'DescribeSubnetsResponse').replace('vpcSet', 'subnetSet').replace('"VpcId"', '"SubnetId"').replace('"vpcId"', '"subnetId"').replace('"InstanceTenancy":{{ toJson (index . "instanceTenancy") }}', '"MapPublicIpOnLaunch.Value":{{ toBool (index . "mapPublicIpOnLaunch") }}')
DEBUG_OPERATION = """  /?Version=2016-11-15:
    post:
      operationId: POST_DebugCorrelation
      parameters:
        - {name: VpcId, in: body, required: false, schema: {type: string}}
        - {name: SubnetId, in: body, required: false, schema: {type: string}}
      requestBody:
        content:
          application/x-www-form-urlencoded:
            schema:
              type: object
              properties:
                DebugCorrelation: {type: string, nullable: true}
      responses:
        '200': {description: Debug label reconciled}
"""

def add_debug_method(resource, name, identity):
    program = '{{ $d := jsonMapFromString . }}{{ if index $d "DebugCorrelation" }}Action=CreateTags{{ else }}Action=DeleteTags{{ end }}&ResourceId.1={{ urlquery (index $d "' + identity + '") }}&Tag.1.Key=omnisdk%3Akey{{ if index $d "DebugCorrelation" }}&Tag.1.Value={{ urlquery (index $d "DebugCorrelation") }}{{ end }}'
    method = "        debug_correlation:\n          operation: {$ref: '#/paths/~1?Version=2016-11-15/post'}\n          request:\n            mediaType: application/x-www-form-urlencoded\n            transform:\n              type: golang_template_text_v0.3.0\n              body: '" + program + "'\n          response: {openAPIDocKey: '200'}\n"
    resource = resource.replace('      methods:\n', '      methods:\n' + method, 1)
    return resource.replace('        update:\n', "        update:\n          - $ref: '#/components/x-stackQL-resources/" + name + "/methods/debug_correlation'\n", 1)



def add_resource_tag_method(resource, name, identity):
    attribute = 'TagSpecification.1.Tag.1.Value'
    program = '{{ $d := jsonMapFromString . }}{{ if ne (index $d "' + attribute + '") nil }}Action=CreateTags{{ else }}Action=DeleteTags{{ end }}&ResourceId.1={{ urlquery (index $d "' + identity + '") }}&Tag.1.Key=resource-key{{ if ne (index $d "' + attribute + '") nil }}&Tag.1.Value={{ urlquery (index $d "' + attribute + '") }}{{ end }}'
    method = "        resource_key_tag:\n          operation: {$ref: '#/paths/~1?Version=2016-11-15/post'}\n          request:\n            mediaType: application/x-www-form-urlencoded\n            transform:\n              type: golang_template_text_v0.3.0\n              body: '" + program + "'\n          response: {openAPIDocKey: '200'}\n"
    route = "      updates:\n        - priority: 100\n          requires: [TagSpecification.1.Tag.1.Value]\n          permits: [TagSpecification.1.Tag.1.Value]\n          target: {$ref: '#/components/x-stackQL-resources/" + name + "/methods/resource_key_tag'}\n"
    resource = resource.replace('      updates:\n', route, 1)
    resource = resource.replace('      methods:\n', '      methods:\n' + method, 1)
    return resource.replace('        update:\n', "        update:\n          - $ref: '#/components/x-stackQL-resources/" + name + "/methods/resource_key_tag'\n", 1)


def prepare(registry, output, refresh=False):
    source = Path(registry) / 'aws/v00.00.00000'
    target = Path(output) / 'aws/v00.00.00000'
    document = (source / 'services/ec2.yaml').read_text()
    start = document.index('    subnets:\n')
    end = document.index('\n    transit_gateway_multicast_domains:', start)
    resource = document[start:end]
    resource = resource.replace('      methods:\n', '''      updates:
        - priority: 100
          requires: [DebugCorrelation]
          permits: [DebugCorrelation]
          target: {$ref: '#/components/x-stackQL-resources/subnets/methods/debug_correlation'}
        - priority: 100
          requires: [MapPublicIpOnLaunch.Value]
          permits: [MapPublicIpOnLaunch.Value]
          target: {$ref: '#/components/x-stackQL-resources/subnets/methods/modify_subnet_attribute'}
      methods:
''', 1)
    anchor = '        update:\n'
    resource = resource.replace(anchor, anchor + "          - $ref: '#/components/x-stackQL-resources/subnets/methods/modify_subnet_attribute'\n", 1)
    resource = add_debug_method(resource, 'subnets', 'SubnetId')
    resource = add_resource_tag_method(resource, 'subnets', 'SubnetId')
    document = document[:start] + resource + document[end:]
    # Expose the observed boolean under the same attribute name as the EC2 Query input.
    start = document.index('    SubnetDisplay:\n')
    end = start + re.search(r'\n    \S', document[start + 5:]).start() + 5
    schema = document[start:end]
    old = '        MapPublicIpOnLaunch:\n'
    if schema.count(old) != 1:
        raise ValueError('SubnetDisplay public-IP field differs from the supported corpus')
    schema = schema.replace(old, '        TagSpecification.1.Tag.1.Value: {type: string}\n        DebugCorrelation: {type: string}\n        MapPublicIpOnLaunch.Value:\n', 1)
    document = document[:start] + schema + document[end:]
    start = document.index('    subnets:\n')
    end = start + re.search(r'\n    \S', document[start + 5:]).start() + 5
    resource = document[start:end]
    begin = resource.index('        describe_subnets:\n')
    finish = begin + re.search(r'\n        \S', resource[begin + 9:]).start() + 9
    read = resource[begin:finish]
    read = read.replace('type: schema_driven_xml_v0.1.0',
        'type: golang_template_mxj_v0.2.0\n              body: |\n' +
        '\n'.join('                ' + line for line in SUBNET_READ.splitlines()))
    resource = resource[:begin] + read + resource[finish:]
    document = document[:start] + resource + document[end:]
    # Name is intent, not a create-only TagSpecification parameter. Existing template DSLs
    # project the observed Name tag and shape CreateTags without AWS logic in the SDK.
    start = document.index('    vpcs:\n')
    end = start + re.search(r'\n    \S', document[start + 5:]).start() + 5
    resource = document[start:end]
    resource = resource.replace('      methods:\n', NAME_METHOD, 1)
    resource = add_debug_method(resource, 'vpcs', 'VpcId')
    resource = add_resource_tag_method(resource, 'vpcs', 'VpcId')
    read_start = resource.index('        describe_vpcs:\n')
    read_end = read_start + re.search(r'\n        \S', resource[read_start + 9:]).start() + 9
    read = resource[read_start:read_end]
    read = read.replace('type: schema_driven_xml_v0.1.0',
                        'type: golang_template_mxj_v0.2.0\n              body: |\n' +
                        '\n'.join('                ' + line for line in NAME_READ.splitlines()))
    resource = resource[:read_start] + read + resource[read_end:]
    resource = resource.replace('        update:\n',
        "        update:\n          - $ref: '#/components/x-stackQL-resources/vpcs/methods/set_name'\n", 1)
    document = document[:start] + resource + document[end:]
    document = document.replace('    VpcDisplay:\n      type: object\n      properties:\n',
        '    VpcDisplay:\n      type: object\n      properties:\n        Name: {type: string}\n        TagSpecification.1.Tag.1.Value: {type: string}\n        DebugCorrelation: {type: string}\n', 1)
    start = document.index('  /?Action=CreateTags&Version=2016-11-15:\n')
    post = document.index('    post:\n', start)
    end = document.index('\n  /', post)
    document = document[:post] + NAME_OPERATION + document[end:]
    document = document.replace('paths:\n', 'paths:\n' + DEBUG_OPERATION, 1)
    target.mkdir(parents=True, exist_ok=refresh)
    (target / 'services').mkdir(exist_ok=refresh)
    shutil.copyfile(source / 'provider.yaml', target / 'provider.yaml')
    (target / 'services/ec2.yaml').write_text(document)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--registry', required=True)
    parser.add_argument('--out', required=True, help='fresh isolated registry directory')
    parser.add_argument('--refresh', action='store_true', help='regenerate the existing example overlay from its source')
    options = parser.parse_args()
    prepare(options.registry, options.out, options.refresh)
    print(options.out)
