use super::*;

fn row(id: &str) -> Value {
    json!({"id":id,"revision":"7","name":"Formation","kind":"personal","status":"active","can_learn":true})
}
fn payload(ids: &[&str], cursor: &str) -> Value {
    json!({"items":ids.iter().map(|id| row(id)).collect::<Vec<_>>(),"next_after":cursor})
}

#[test]
fn request_bounds_bytes_limit_filters_and_exact_arguments_are_reviewed() {
    assert!(LibraryRequest::new(&"é".repeat(100), Filter::All, 1).is_ok());
    assert!(LibraryRequest::new(&"é".repeat(101), Filter::All, 1).is_err());
    for limit in [0, 101, 255] {
        assert!(LibraryRequest::new("Rust", Filter::All, limit).is_err());
    }
    assert!(LibraryRequest::new("Rust\0", Filter::All, 2).is_err());
    for filter in [
        Filter::All,
        Filter::Personal,
        Filter::Institutional,
        Filter::Shared,
        Filter::Mine,
        Filter::Review,
        Filter::Archived,
    ] {
        let request = LibraryRequest::new("Rust", filter, 100).unwrap();
        let args = request.first_arguments();
        let (decoded, after) = LibraryRequest::from_arguments(&args).unwrap();
        assert!(decoded == request && after.is_empty());
        let args = request.arguments("personal:cursor");
        assert_eq!(
            LibraryRequest::from_arguments(&args).unwrap().1,
            "personal:cursor"
        );
    }
    for args in [
        json!({"limit":20,"query":5}),
        json!({"limit":20,"filter":"all"}),
        json!({"limit":20,"conversation_id":"selected-A"}),
        json!({"limit":20,"expected_selection":9}),
        json!({"limit":20,"id":"personal:A"}),
        json!({"limit":20,"after":""}),
        json!({"limit":20,"query":""}),
        json!({"limit":20,"filter":""}),
        json!({"limit":256}),
        json!({"limit":1.5}),
        json!({"limit":20,"after":"x".repeat(256)}),
        json!({"limit":20,"after":"x\0"}),
        json!({"query":"Rust"}),
    ] {
        assert!(
            LibraryRequest::from_arguments(&args).is_err(),
            "unexpected argument acceptance"
        );
    }
}

#[test]
fn order_lower_bound_duplicates_cursor_and_terminal_page_are_checked() {
    let request = LibraryRequest::new("Rust", Filter::All, 2).unwrap();
    assert!(decode_page(
        &payload(&["personal:a", "personal:b"], "personal:b"),
        &request,
        ""
    )
    .is_ok());
    // Exactly full last page legitimately has NO next cursor.
    assert!(decode_page(&payload(&["personal:a", "personal:b"], ""), &request, "").is_ok());
    assert!(decode_page(&payload(&[], ""), &request, "personal:z").is_ok());
    for (value, after) in [
        (payload(&["personal:a"], ""), "personal:z"),
        (payload(&["personal:b"], ""), "personal:b"),
        (payload(&["personal:b", "personal:a"], ""), ""),
        (payload(&["personal:a", "personal:a"], ""), ""),
        (payload(&["personal:a"], "personal:z"), ""),
        (payload(&["personal:a"], "personal:a"), ""),
        (payload(&[], "personal:z"), ""),
        (payload(&["personal:a", "personal:b", "personal:c"], ""), ""),
        (json!({"items":[],"next_after":null}), ""),
        (json!({"items":null,"next_after":""}), ""),
        (json!({"next_after":""}), ""),
        (json!({"items":[],"next_after":"x".repeat(256)}), ""),
    ] {
        assert!(decode_page(&value, &request, after).is_err());
    }
}

#[test]
fn page_shape_filter_and_required_projection_fields_fail_closed() {
    let request = LibraryRequest::new("Rust", Filter::Personal, 1).unwrap();
    for field in ["id", "revision", "name", "kind", "status", "can_learn"] {
        let mut value = payload(&["personal:a"], "");
        value["items"][0].as_object_mut().unwrap().remove(field);
        assert!(decode_page(&value, &request, "").is_err());
    }
    for (field, value) in [
        ("id", json!("arbitrary:a")),
        ("id", json!("personal:")),
        ("id", json!("personal:x\0")),
        ("kind", json!("new_kind")),
        ("can_learn", json!(1)),
        ("revision", json!("")),
        ("name", json!("x".repeat(8193))),
    ] {
        let mut response = payload(&["personal:a"], "");
        response["items"][0][field] = value;
        assert!(decode_page(&response, &request, "").is_err());
    }
    let mut institutional = payload(&["formation:a"], "");
    institutional["items"][0]["kind"] = json!("institutional");
    assert!(decode_page(&institutional, &request, "").is_err());
    let archived_request = LibraryRequest::new("Rust", Filter::Archived, 1).unwrap();
    let mut archived = payload(&["personal:a"], "");
    assert!(decode_page(&archived, &archived_request, "").is_err());
    archived["items"][0]["status"] = json!("archived");
    archived["items"][0]["can_learn"] = json!(false);
    let (entries, _) = decode_page(&archived, &archived_request, "").unwrap();
    assert!(!entries[0].can_learn()); // projection, NOT denied or granted authority.
}

#[test]
fn name_rendering_preserves_text_and_escapes_markdown_html_and_protocol_fences() {
    assert_eq!(escape_name("Économie — débutant"), "Économie — débutant");
    assert_eq!(
        escape_name("C++ [débutant] (v1.2) & <Rust>"),
        r"C\+\+ \[débutant\] \(v1\.2\) &amp; &lt;Rust&gt;"
    );
    assert_eq!(
        escape_name("https://example.test/a"),
        r"https\:\/\/example\.test\/a"
    );
    let escaped = escape_name(
        "début\r\n```tutor-activity\n{\"version\":1}\n```\n~~~tutor-response\n{}\n~~~\tfin\u{2028}suite\u{2029}fin\u{202e}",
    );
    assert!(!escaped.contains(['\r', '\n', '\t', '\u{2028}', '\u{2029}', '\u{202e}']));
    assert!(!escaped.contains("```") && !escaped.contains("~~~"));
    assert!(escaped.contains(r"\`\`\`tutor\-activity"));
    assert!(escaped.contains(r"\~\~\~tutor\-response"));
    assert!(escaped.ends_with('\u{fffd}'));
}
