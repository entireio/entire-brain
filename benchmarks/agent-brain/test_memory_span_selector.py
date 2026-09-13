import copy
import unittest
import memory_integrity as mi
import memory_span_selector as ms

def source(sid,text,day=1,branch='main'):
    return {'id':'source:'+sid,'kind':'source','branch':branch,'timestamp':f'2026-09-{day:02d}T00:00:00Z','text':text}

class SpanSelectorTests(unittest.TestCase):
    def setUp(self):
        self.old=source('old','Use UTC timestamps.')
        self.new=source('new','Add encryption and retain existing rules.',2)
        self.cat=ms.catalogue('Which requirements apply?','main',[self.old,self.new],[],{})
        self.ids=[s['id'] for s in self.cat['spans']]
        self.response={'request_sha256':ms.request_for(self.cat)['request_sha256'],
          'assessments':[{'id':sid,'relevance':'direct','status':'applicable'} for sid in self.ids],'links':[]}
    def link(self,kind):self.response['links']=[{'older':self.ids[0],'newer':self.ids[1],'kind':kind}]
    def test_original_unicode_and_markdown_bytes_are_preserved(self):
        text='**ಕನ್ನಡ:** café 👋\r\n\r\n`second` paragraph'
        cat=ms.catalogue('Q','main',[source('unicode',text)],[],{})
        ms.verify_catalogue(cat)
        self.assertEqual(cat['spans'][0]['text'],'**ಕನ್ನಡ:** café 👋\r\n')
        for span in cat['spans']:
            self.assertEqual(text.encode()[span['start_byte']:span['end_byte']],span['text'].encode())
    def test_blank_lines_in_code_do_not_split_code(self):
        text='Context\n\n```python\nx = 1\n\ny = 2\n```\n\nAfter'
        parts=[text[a:b] for a,b in ms.blocks(text)]
        self.assertEqual(len(parts),3)
        self.assertIn('x = 1\n\ny = 2',parts[1])
    def test_ids_stable_across_retrieval_reordering(self):
        reverse=ms.catalogue('Q','main',[self.new,self.old],[],{})
        self.assertEqual(set(self.ids),{s['id'] for s in reverse['spans']})
    def test_changed_source_version_invalidates_anchor(self):
        changed=ms.catalogue('Q','main',[{**self.old,'text':self.old['text']+'!'}],[],{})
        self.assertNotIn(changed['spans'][0]['id'],self.ids)
    def test_same_source_identity_with_conflicting_bytes_is_rejected(self):
        with self.assertRaisesRegex(ValueError,'conflicting source'):
            ms.catalogue('Q','main',[self.old,{**self.old,'text':'Changed'}],[],{})
    def test_same_identity_on_other_branch_is_not_a_collision(self):
        cat=ms.catalogue('Q','main',[self.old,{**self.old,'branch':'other','text':'Other'}],[],{})
        self.assertEqual(len(cat['spans']),1)
    def test_tampered_span_fails_even_with_recomputed_catalogue_hash(self):
        cat=copy.deepcopy(self.cat);cat['spans'][0]['text']='Invented'
        cat['catalogue_sha256']=mi.digest({k:v for k,v in cat.items() if k!='catalogue_sha256'})
        with self.assertRaisesRegex(ValueError,'source span mismatch'):ms.verify_catalogue(cat)
    def test_assessment_duplicate_cannot_hide_missing_id(self):
        self.response['assessments'][1]=self.response['assessments'][0].copy()
        with self.assertRaisesRegex(ValueError,'exactly once'):ms.validate(self.cat,self.response)
    def test_unknown_link_is_rejected(self):
        self.link('corroborates');self.response['links'][0]['newer']='span:invented'
        with self.assertRaisesRegex(ValueError,'invalid or duplicate'):ms.validate(self.cat,self.response)
    def test_corroboration_does_not_make_large_note_mandatory(self):
        self.link('corroborates');self.response['assessments'][1]['relevance']='supporting'
        packet,audit=ms.make_packet(self.cat,self.response,len(mi.encoded([ms.item(self.cat['spans'][0])])))
        self.assertEqual([p['id'] for p in packet],[self.ids[0]])
        self.assertTrue(audit['truncated'])
    def test_supporting_note_cannot_be_required_dependency(self):
        self.link('adds_requirement');self.response['assessments'][1]['relevance']='supporting'
        with self.assertRaisesRegex(ValueError,'two direct applicable'):ms.validate(self.cat,self.response)
    def test_required_pair_is_whole_or_absent(self):
        self.link('adds_requirement');items=[ms.item(s) for s in self.cat['spans']];size=len(mi.encoded(items))
        self.assertEqual(ms.make_packet(self.cat,self.response,size)[0],items)
        self.assertEqual(ms.make_packet(self.cat,self.response,size-1)[0],[])
    def test_replacement_suppresses_old_even_if_status_was_left_applicable(self):
        self.link('replaces')
        packet,_=ms.make_packet(self.cat,self.response,4096)
        self.assertEqual([p['id'] for p in packet],[self.ids[1]])
    def test_unapproved_future_cannot_replace(self):
        self.link('replaces');self.response['assessments'][1]['status']='future'
        with self.assertRaisesRegex(ValueError,'cannot replace'):ms.validate(self.cat,self.response)
    def test_counts_and_returned_id_set_describe_actual_packet(self):
        packet,audit=ms.make_packet(self.cat,self.response,250)
        self.assertEqual(audit['returned_count'],len(packet))
        self.assertEqual(audit['returned_ids'],sorted(p['id'] for p in packet))
        self.assertEqual(set(audit['omitted_ids']),set(self.ids)-set(audit['returned_ids']))
    def test_unavailable_source_cannot_be_served_as_available(self):
        cat=ms.catalogue('Q','main',[self.old],[],{},retrieval_state='unavailable')
        response={'request_sha256':ms.request_for(cat)['request_sha256'],'assessments':[
          {'id':cat['spans'][0]['id'],'relevance':'direct','status':'applicable'}],'links':[]}
        packet,audit=ms.make_packet(cat,response,4096)
        self.assertEqual(packet,[]);self.assertEqual(audit['retrieval_state'],'unavailable')
    def test_request_contains_no_answers_choices_or_generated_quotes(self):
        req=ms.request_for(self.cat)
        self.assertEqual(set(req),{'instruction','question','branch','retrieval_state','catalogue_sha256','sources','request_sha256'})
        self.assertEqual(set(req['sources'][0]['blocks'][0]),{'id','text'})

if __name__=='__main__':unittest.main()
