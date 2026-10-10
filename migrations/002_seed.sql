INSERT INTO skus (id, code, name, image_url) VALUES
 (1, 'SR-KUR-001', 'Gulab embroidered kurta', 'https://images.unsplash.com/photo-1583391733956-6c78276477ea?w=400&auto=format&fit=crop'),
 (2, 'SR-SET-002', 'Neel cotton kurta set', 'https://images.unsplash.com/photo-1610030469983-98e550d6193c?w=400&auto=format&fit=crop'),
 (3, 'SR-DUP-003', 'Chand silk dupatta', 'https://images.unsplash.com/photo-1610030469668-8e9f641aeb5a?w=400&auto=format&fit=crop'),
 (4, 'SR-KUR-004', 'Meher block-print kurta', 'https://images.unsplash.com/photo-1583391733981-24c5a63a2019?w=400&auto=format&fit=crop'),
 (5, 'SR-SET-005', 'Noor festive co-ord', 'https://images.unsplash.com/photo-1617627143750-d86bc21e42bb?w=400&auto=format&fit=crop');

INSERT INTO parties (id, name, phone) VALUES
 (1, 'Asha Embroidery', '+91 98100 12001'),
 (2, 'Rafiq Stitching', '+91 98100 12002');

INSERT INTO jobwork_issues (id, party_id, sku_id, qty, issued_on) VALUES
 (1, 1, 1, 100, CURRENT_DATE - 5),
 (2, 2, 2, 80, CURRENT_DATE - 4),
 (3, 1, 3, 60, CURRENT_DATE - 3),
 (4, 2, 4, 120, CURRENT_DATE - 2);

SELECT setval(pg_get_serial_sequence('skus', 'id'), (SELECT max(id) FROM skus));
SELECT setval(pg_get_serial_sequence('parties', 'id'), (SELECT max(id) FROM parties));
SELECT setval(pg_get_serial_sequence('jobwork_issues', 'id'), (SELECT max(id) FROM jobwork_issues));
