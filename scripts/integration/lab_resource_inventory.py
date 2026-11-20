#!/usr/bin/env python3
"""Read-only inventory of the identified local lab, including allocations."""
import json
import logging
import os
from urllib.parse import urlsplit


def inventory():
    logging.disable(logging.CRITICAL)
    from keystoneauth1.identity import v3
    from keystoneauth1 import session
    from openstack.connection import Connection
    url=os.environ['OS_AUTH_URL'].rstrip('/')
    if url not in ('http://192.168.122.10/identity','http://192.168.122.10/identity/v3'):
        raise ValueError('identified local Keystone required')
    if not url.endswith('/v3'):url+='/v3'
    auth=v3.Password(auth_url=url,username=os.environ['OS_USERNAME'],password=os.environ['OS_PASSWORD'],
        project_name=os.environ['OS_PROJECT_NAME'],user_domain_name=os.environ.get('OS_USER_DOMAIN_NAME','Default'),
        project_domain_name=os.environ.get('OS_PROJECT_DOMAIN_NAME','Default'))
    sess=session.Session(auth=auth,timeout=15)
    catalog=auth.get_access(sess).service_catalog.catalog
    for service in catalog:
        if service['type'] in ('compute','image','network','volumev3','placement'):
            for endpoint in service['endpoints']:
                if urlsplit(endpoint['url']).hostname not in ('192.168.122.10','127.0.0.1','localhost','tt-openstack-lab'):
                    raise ValueError('external service endpoint refused')
    c=Connection(session=sess,region_name='RegionOne',image_api_version='2',image_endpoint_override='http://192.168.122.10/image/v2')
    result={'servers':[{'id':s.id,'name':s.name,'status':s.status} for s in c.compute.servers(all_projects=True)],
        'images':[{'id':i.id,'name':i.name,'status':i.status,'size':i.size,'hash_algo':i.hash_algo,'hash':i.hash_value} for i in c.image.images()],
        'flavors':[{'id':f.id,'name':f.name,'ram':f.ram,'disk':f.disk,'vcpus':f.vcpus} for f in c.compute.flavors()],
        'networks':[{'id':n.id,'name':n.name,'external':n.is_router_external} for n in c.network.networks()],
        'volumes':[{'id':v.id,'status':v.status,'attachments':v.attachments} for v in c.block_storage.volumes(all_projects=True)]}
    result['allocations']={s['id']:sess.get('http://192.168.122.10/placement/allocations/'+s['id'],
        headers={'OpenStack-API-Version':'placement 1.28'}).json() for s in result['servers']}
    return result


if __name__=='__main__':
    print(json.dumps(inventory(),indent=2))
