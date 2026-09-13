import unittest
import memory_integrity as mi
import memory_evidence_selector as selector

class SelectorTests(unittest.TestCase):
    def setUp(self):
        self.old={'id':'source:old','branch':'main','kind':'source','timestamp':'2026-09-01T00:00:00Z','text':'Use UTC timestamps.'}
        self.new={'id':'source:new','branch':'main','kind':'source','timestamp':'2026-09-02T00:00:00Z','text':'Add encryption; retain UTC.'}
        self.request=selector.build_request('What rules apply?','main',[self.old,self.new],[],{})
        self.response={'request_sha256':self.request['request_sha256'],'assessments':[
            {'id':s['id'],'relevance':'direct','status':'applicable','quote':s['text']} for s in (self.old,self.new)],'links':[]}
    def link(self,kind):
        return {'older':self.old['id'],'newer':self.new['id'],'kind':kind,'older_quote':self.old['text'],'newer_quote':self.new['text']}
    def test_projection_is_source_only_and_filters_other_branches(self):
        req=selector.build_request('Question','main',[{**self.new,'branch':'experiment'}],[],{})
        self.assertEqual(req['candidates'],[])
        self.assertEqual(set(req),{'instruction','question','branch','candidates','request_sha256'})
    def test_relevance_protects_old_rule_over_recent_distraction(self):
        self.response['assessments'][1]['relevance']='unrelated'
        packet,_=selector.make_packet(self.request,self.response,[],{},len(mi.encoded([self.old])))
        self.assertEqual(packet,[self.old])
    def test_replacement_link_overrides_old_retrieval_rank(self):
        self.response['links']=[self.link('replaces')]
        packet,_=selector.make_packet(self.request,self.response,[],{},len(mi.encoded([self.new])))
        self.assertEqual(packet,[self.new])
    def test_additive_dependency_is_whole_or_absent(self):
        self.response['links']=[self.link('extends')]
        budget=len(mi.encoded([self.old,self.new]))
        self.assertEqual(selector.make_packet(self.request,self.response,[],{},budget)[0],[self.old,self.new])
        self.assertEqual(selector.make_packet(self.request,self.response,[],{},budget-1)[0],[])
    def test_missing_or_duplicate_assessment_rejected(self):
        self.response['assessments'].pop()
        with self.assertRaisesRegex(ValueError,'exactly once'):selector.validate(self.request,self.response)
    def test_fact_backfill_cannot_bypass_additive_dependency(self):
        self.response['links']=[self.link('extends')]
        facts=[{'id':'old-rule','branch':'main','text':'UTC','source_ids':['old']}]
        sources={'old':{**self.old,'id':'old'}}
        budget=len(mi.encoded([self.old,self.new]))-1
        self.assertEqual(selector.make_packet(self.request,self.response,facts,sources,budget)[0],[])
    def test_invented_quote_rejected(self):
        self.response['assessments'][0]['quote']='invented rule'
        with self.assertRaisesRegex(ValueError,'exact source span'):selector.validate(self.request,self.response)
    def test_future_or_speculation_cannot_replace_current_policy(self):
        self.response['links']=[self.link('replaces')]
        for status in ('future','proposal'):
            self.response['assessments'][1]['status']=status
            with self.assertRaisesRegex(ValueError,'cannot replace'):selector.validate(self.request,self.response)
    def test_backward_and_cross_branch_links_rejected(self):
        self.response['links']=[self.link('replaces')]
        self.request['candidates'][1]['timestamp']=self.old['timestamp']
        with self.assertRaisesRegex(ValueError,'known time'):selector.validate(self.request,self.response)
        self.request['candidates'][1]['branch']='experiment'
        with self.assertRaisesRegex(ValueError,'cross-branch'):selector.validate(self.request,self.response)
    def test_unknown_link_endpoint_rejected(self):
        self.response['links']=[{**self.link('extends'),'newer':'source:missing'}]
        with self.assertRaisesRegex(ValueError,'invalid or duplicate'):selector.validate(self.request,self.response)

if __name__=='__main__':unittest.main()
